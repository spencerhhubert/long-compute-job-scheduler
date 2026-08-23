package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
)

type WorkerCredential struct {
	WorkerID string
	Name     string
}

func (s *Store) CreateWorker(ctx context.Context, name, rawToken string, labels map[string]string) (WorkerCredential, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return WorkerCredential{}, errors.New("worker name must contain between 1 and 100 characters")
	}
	if len(rawToken) < 32 {
		return WorkerCredential{}, errors.New("worker token must contain at least 32 characters")
	}
	workerID, err := id.New("wrk")
	if err != nil {
		return WorkerCredential{}, err
	}
	capacity, err := json.Marshal(domain.WorkerCapacity{Labels: labels})
	if err != nil {
		return WorkerCredential{}, fmt.Errorf("encode worker labels: %w", err)
	}
	now := formatTime(s.now())
	hash := sha256.Sum256([]byte(rawToken))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkerCredential{}, fmt.Errorf("begin create worker: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workers(id, name, status, capacity_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, workerID, name, domain.WorkerOffline, capacity, now, now); err != nil {
		return WorkerCredential{}, fmt.Errorf("create worker: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO worker_tokens(token_hash, worker_id, created_at)
		VALUES (?, ?, ?)
	`, hash[:], workerID, now); err != nil {
		return WorkerCredential{}, fmt.Errorf("create worker token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkerCredential{}, fmt.Errorf("commit worker: %w", err)
	}
	return WorkerCredential{WorkerID: workerID, Name: name}, nil
}

func (s *Store) AuthenticateWorkerToken(ctx context.Context, rawToken string) (WorkerCredential, error) {
	hash := sha256.Sum256([]byte(rawToken))
	var worker WorkerCredential
	err := s.db.QueryRowContext(ctx, `
		SELECT w.id, w.name
		FROM worker_tokens AS t
		JOIN workers AS w ON w.id = t.worker_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL
	`, hash[:]).Scan(&worker.WorkerID, &worker.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerCredential{}, ErrNotFound
	}
	if err != nil {
		return WorkerCredential{}, fmt.Errorf("authenticate worker token: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE worker_tokens SET last_used_at = ? WHERE token_hash = ?`, formatTime(s.now()), hash[:]); err != nil {
		return WorkerCredential{}, fmt.Errorf("record worker token use: %w", err)
	}
	return worker, nil
}

func (s *Store) SyncWorker(ctx context.Context, workerID string, request domain.WorkerSyncRequest) (domain.WorkerSyncResponse, error) {
	now := s.now().UTC()
	capacityJSON, err := json.Marshal(request.Capacity)
	if err != nil {
		return domain.WorkerSyncResponse{}, fmt.Errorf("encode worker capacity: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.WorkerSyncResponse{}, fmt.Errorf("begin worker sync: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE workers
		SET status = ?, agent_version = ?, session_id = ?, capacity_json = ?, last_seen_at = ?, updated_at = ?
		WHERE id = ?
	`, domain.WorkerOnline, request.AgentVersion, request.SessionID, capacityJSON, formatTime(now), formatTime(now), workerID)
	if err != nil {
		return domain.WorkerSyncResponse{}, fmt.Errorf("update worker: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return domain.WorkerSyncResponse{}, ErrNotFound
	}

	for _, event := range request.Events {
		if err := recordWorkerEvent(ctx, tx, workerID, event, now); err != nil {
			return domain.WorkerSyncResponse{}, err
		}
	}
	if err := offerJobs(ctx, tx, workerID, request.Capacity, now); err != nil {
		return domain.WorkerSyncResponse{}, err
	}
	commands, err := offeredCommands(ctx, tx, workerID)
	if err != nil {
		return domain.WorkerSyncResponse{}, err
	}
	var acceptedThrough uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM worker_events WHERE worker_id = ?`, workerID).Scan(&acceptedThrough); err != nil {
		return domain.WorkerSyncResponse{}, fmt.Errorf("read worker acknowledgement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.WorkerSyncResponse{}, fmt.Errorf("commit worker sync: %w", err)
	}
	return domain.WorkerSyncResponse{
		ServerTime: now, AcceptedThrough: acceptedThrough,
		NextSyncSeconds: 5, Commands: commands,
	}, nil
}

func recordWorkerEvent(ctx context.Context, tx *sql.Tx, workerID string, event domain.WorkerEvent, recordedAt time.Time) error {
	if event.Sequence == 0 || event.EventID == "" || event.AttemptID == "" {
		return errors.New("worker event requires sequence, event_id, and attempt_id")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode worker event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO worker_events(worker_id, sequence, event_id, attempt_id, kind, payload_json, occurred_at, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, workerID, event.Sequence, event.EventID, event.AttemptID, event.Kind, payload, formatTime(event.OccurredAt), formatTime(recordedAt))
	if err != nil {
		return fmt.Errorf("record worker event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect worker event insert: %w", err)
	}
	if inserted == 0 {
		var existing string
		if err := tx.QueryRowContext(ctx, `SELECT event_id FROM worker_events WHERE worker_id = ? AND sequence = ?`, workerID, event.Sequence).Scan(&existing); err != nil {
			return fmt.Errorf("read duplicate worker event: %w", err)
		}
		if existing != event.EventID {
			return fmt.Errorf("worker event sequence %d conflicts with event %s", event.Sequence, existing)
		}
		return nil
	}
	return applyWorkerEvent(ctx, tx, workerID, event, recordedAt)
}

func applyWorkerEvent(ctx context.Context, tx *sql.Tx, workerID string, event domain.WorkerEvent, recordedAt time.Time) error {
	var state domain.AttemptState
	var jobID string
	var attemptNumber uint32
	var specJSON []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT a.state, a.job_id, a.attempt_number, j.spec_json
		FROM attempts AS a JOIN jobs AS j ON j.id = a.job_id
		WHERE a.id = ? AND a.worker_id = ?
	`, event.AttemptID, workerID).Scan(&state, &jobID, &attemptNumber, &specJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("event references an attempt not assigned to this worker")
		}
		return fmt.Errorf("read event attempt: %w", err)
	}
	var spec domain.JobSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return fmt.Errorf("decode event job: %w", err)
	}

	switch event.Kind {
	case domain.WorkerEventAttemptAccepted:
		if state != domain.AttemptOffered {
			return fmt.Errorf("cannot accept attempt in state %s", state)
		}
		_, err := tx.ExecContext(ctx, `UPDATE attempts SET state = ?, revision = revision + 1, accepted_at = ? WHERE id = ?`, domain.AttemptAccepted, formatTime(event.OccurredAt), event.AttemptID)
		return err
	case domain.WorkerEventAttemptStarted:
		if state != domain.AttemptAccepted && state != domain.AttemptOffered {
			return fmt.Errorf("cannot start attempt in state %s", state)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET state = ?, revision = revision + 1, started_at = ? WHERE id = ?`, domain.AttemptRunning, formatTime(event.OccurredAt), event.AttemptID); err != nil {
			return fmt.Errorf("start attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND state = ?`, domain.JobRunning, formatTime(recordedAt), jobID, domain.JobQueued); err != nil {
			return fmt.Errorf("start job: %w", err)
		}
		return appendControlEvent(ctx, tx, "attempt_started", jobID, map[string]any{"attempt_id": event.AttemptID, "worker_id": workerID}, recordedAt)
	case domain.WorkerEventAttemptFinished:
		if state != domain.AttemptRunning && state != domain.AttemptAccepted {
			return fmt.Errorf("cannot finish attempt in state %s", state)
		}
		exitCode := -1
		if event.ExitCode != nil {
			exitCode = *event.ExitCode
		}
		if len(event.LogTail) > 64<<10 {
			return errors.New("attempt log tail exceeds 64 KiB")
		}
		attemptState := domain.AttemptFailed
		jobState := domain.JobFailed
		if exitCode == 0 && event.Error == "" {
			attemptState = domain.AttemptSucceeded
			jobState = domain.JobSucceeded
		} else if attemptNumber < spec.Retry.MaxAttempts {
			jobState = domain.JobQueued
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempts SET state = ?, revision = revision + 1, finished_at = ?, exit_code = ?, error = ?, log_uri = ?, log_tail = ? WHERE id = ?
		`, attemptState, formatTime(event.OccurredAt), exitCode, event.Error, event.LogURI, event.LogTail, event.AttemptID); err != nil {
			return fmt.Errorf("finish attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, jobState, formatTime(recordedAt), jobID); err != nil {
			return fmt.Errorf("finish job: %w", err)
		}
		for _, artifact := range event.Artifacts {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO artifacts(attempt_id, name, uri, size_bytes, sha256, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, event.AttemptID, artifact.Name, artifact.URI, artifact.SizeBytes, artifact.SHA256, formatTime(recordedAt)); err != nil {
				return fmt.Errorf("record artifact: %w", err)
			}
		}
		return appendControlEvent(ctx, tx, "attempt_finished", jobID, map[string]any{"attempt_id": event.AttemptID, "worker_id": workerID, "state": attemptState, "exit_code": exitCode}, recordedAt)
	case domain.WorkerEventMetricSample:
		if event.Metric == nil || event.Metric.Name == "" || math.IsNaN(event.Metric.Value) || math.IsInf(event.Metric.Value, 0) {
			return errors.New("metric event requires a named finite sample")
		}
		if !definedMetric(spec.Metrics, event.Metric.Name, event.Metric.Value) {
			return fmt.Errorf("metric %q is not defined by the job or uses an invalid scale", event.Metric.Name)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO metric_samples(attempt_id, event_id, name, value, step, observed_at, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, event.AttemptID, event.EventID, event.Metric.Name, event.Metric.Value, event.Metric.Step, formatTime(event.Metric.ObservedAt), formatTime(recordedAt)); err != nil {
			return fmt.Errorf("record metric sample: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown worker event kind %q", event.Kind)
	}
}

func definedMetric(metrics []domain.MetricDefinition, name string, value float64) bool {
	for _, metric := range metrics {
		if metric.Name == name {
			return metricValueFits(metric, value)
		}
	}
	if metric, ok := domain.SystemMetricDefinition(name); ok {
		return metricValueFits(metric, value)
	}
	return false
}

func metricValueFits(metric domain.MetricDefinition, value float64) bool {
	return metric.Format != domain.MetricFormatPercent || (value >= 0 && value <= 1)
}

func appendControlEvent(ctx context.Context, tx *sql.Tx, kind, resourceID string, value any, now time.Time) error {
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO events(event_id, kind, resource_id, payload_json, occurred_at, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, eventID, kind, resourceID, payload, formatTime(now), formatTime(now))
	return err
}

type resourceAllocation struct {
	CPU            uint32
	MemoryBytes    uint64
	GPUSharedSlots uint32
	GPUExclusive   bool
	GPUInUse       bool
	ActiveAttempts uint32
}

func offerJobs(ctx context.Context, tx *sql.Tx, workerID string, capacity domain.WorkerCapacity, now time.Time) error {
	if capacity.MaxParallel == 0 {
		capacity.MaxParallel = 1
	}
	if capacity.GPUSharedSlots == 0 {
		capacity.GPUSharedSlots = capacity.GPUCount
	}
	allocation, err := currentAllocation(ctx, tx, workerID)
	if err != nil {
		return err
	}
	if allocation.ActiveAttempts >= capacity.MaxParallel {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT j.id, j.state, j.revision, j.spec_json, j.created_at, j.updated_at
		FROM jobs AS j
		WHERE j.state = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM attempts AS a
		    WHERE a.job_id = j.id AND a.state IN (?, ?, ?)
		  )
		ORDER BY j.priority DESC, j.created_at, j.id
		LIMIT 100
	`, domain.JobQueued, domain.AttemptOffered, domain.AttemptAccepted, domain.AttemptRunning)
	if err != nil {
		return fmt.Errorf("list schedulable jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]domain.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return fmt.Errorf("scan schedulable job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read schedulable jobs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, job := range jobs {
		if allocation.ActiveAttempts >= capacity.MaxParallel {
			break
		}
		if !jobFits(job.Spec, capacity, allocation) {
			continue
		}
		attemptID, err := id.New("att")
		if err != nil {
			return err
		}
		commandID, err := id.New("cmd")
		if err != nil {
			return err
		}
		var attemptNumber uint32
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM attempts WHERE job_id = ?`, job.ID).Scan(&attemptNumber); err != nil {
			return fmt.Errorf("choose attempt number: %w", err)
		}
		leaseExpiresAt := now.Add(2 * time.Minute)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempts(id, job_id, attempt_number, worker_id, command_id, state, revision, offered_at, lease_expires_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		`, attemptID, job.ID, attemptNumber, workerID, commandID, domain.AttemptOffered, formatTime(now), formatTime(leaseExpiresAt)); err != nil {
			return fmt.Errorf("offer attempt: %w", err)
		}
		if err := appendControlEvent(ctx, tx, "attempt_offered", job.ID, map[string]any{"attempt_id": attemptID, "worker_id": workerID}, now); err != nil {
			return err
		}
		addAllocation(&allocation, job.Spec.Resources)
		allocation.ActiveAttempts++
	}
	return nil
}

func currentAllocation(ctx context.Context, tx *sql.Tx, workerID string) (resourceAllocation, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT j.spec_json
		FROM attempts AS a JOIN jobs AS j ON j.id = a.job_id
		WHERE a.worker_id = ? AND a.state IN (?, ?, ?)
	`, workerID, domain.AttemptOffered, domain.AttemptAccepted, domain.AttemptRunning)
	if err != nil {
		return resourceAllocation{}, fmt.Errorf("read worker allocation: %w", err)
	}
	defer rows.Close()
	var allocation resourceAllocation
	for rows.Next() {
		var specJSON []byte
		if err := rows.Scan(&specJSON); err != nil {
			return resourceAllocation{}, err
		}
		var spec domain.JobSpec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return resourceAllocation{}, err
		}
		addAllocation(&allocation, spec.Resources)
		allocation.ActiveAttempts++
	}
	return allocation, rows.Err()
}

func addAllocation(allocation *resourceAllocation, resources domain.ResourceRequest) {
	allocation.CPU += resources.CPU
	allocation.MemoryBytes += resources.MemoryBytes
	for _, gpu := range resources.GPUs {
		allocation.GPUInUse = true
		if gpu.Sharing == domain.GPUExclusive {
			allocation.GPUExclusive = true
		} else {
			allocation.GPUSharedSlots += gpu.Count
		}
	}
}

func jobFits(spec domain.JobSpec, capacity domain.WorkerCapacity, allocation resourceAllocation) bool {
	for name, value := range spec.Constraints.Labels {
		if capacity.Labels[name] != value {
			return false
		}
	}
	if capacity.CPU > 0 && allocation.CPU+spec.Resources.CPU > capacity.CPU {
		return false
	}
	if capacity.MemoryBytes > 0 && allocation.MemoryBytes+spec.Resources.MemoryBytes > capacity.MemoryBytes {
		return false
	}
	var exclusive, shared uint32
	for _, gpu := range spec.Resources.GPUs {
		if gpu.Sharing == domain.GPUExclusive {
			exclusive += gpu.Count
		} else {
			shared += gpu.Count
		}
	}
	if exclusive > 0 {
		return !allocation.GPUInUse && shared == 0 && exclusive <= capacity.GPUCount
	}
	if shared > 0 {
		return !allocation.GPUExclusive && allocation.GPUSharedSlots+shared <= capacity.GPUSharedSlots
	}
	return true
}

func offeredCommands(ctx context.Context, tx *sql.Tx, workerID string) ([]domain.WorkerCommand, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.command_id, a.id, a.attempt_number, a.lease_expires_at,
		       j.id, j.state, j.revision, j.spec_json, j.created_at, j.updated_at
		FROM attempts AS a JOIN jobs AS j ON j.id = a.job_id
		WHERE a.worker_id = ? AND a.state = ?
		ORDER BY a.offered_at, a.id
	`, workerID, domain.AttemptOffered)
	if err != nil {
		return nil, fmt.Errorf("list worker commands: %w", err)
	}
	defer rows.Close()
	commands := make([]domain.WorkerCommand, 0)
	for rows.Next() {
		var command domain.WorkerCommand
		var job domain.Job
		var specJSON []byte
		var lease, created, updated string
		if err := rows.Scan(&command.CommandID, &command.AttemptID, &command.AttemptNumber, &lease, &job.ID, &job.State, &job.Revision, &specJSON, &created, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(specJSON, &job.Spec); err != nil {
			return nil, err
		}
		var err error
		if command.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, lease); err != nil {
			return nil, err
		}
		if job.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, err
		}
		command.Kind = "run_job"
		command.Job = job
		commands = append(commands, command)
	}
	return commands, rows.Err()
}

func (s *Store) ListWorkers(ctx context.Context, offlineAfter time.Duration) ([]domain.Worker, error) {
	if offlineAfter <= 0 {
		offlineAfter = 30 * time.Second
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, agent_version, capacity_json, last_seen_at, created_at
		FROM workers ORDER BY name, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()
	workers := make([]domain.Worker, 0)
	now := s.now().UTC()
	for rows.Next() {
		var worker domain.Worker
		var capacityJSON []byte
		var lastSeen sql.NullString
		var created string
		if err := rows.Scan(&worker.ID, &worker.Name, &worker.AgentVersion, &capacityJSON, &lastSeen, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(capacityJSON, &worker.Capacity); err != nil {
			return nil, err
		}
		var err error
		worker.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		worker.Status = domain.WorkerOffline
		if lastSeen.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, lastSeen.String)
			if err != nil {
				return nil, err
			}
			worker.LastSeenAt = &parsed
			if now.Sub(parsed) <= offlineAfter {
				worker.Status = domain.WorkerOnline
			}
		}
		workers = append(workers, worker)
	}
	return workers, rows.Err()
}
