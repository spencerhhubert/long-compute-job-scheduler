package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func (s *Store) GetJobDetail(ctx context.Context, jobID string) (domain.JobDetail, error) {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return domain.JobDetail{}, err
	}
	detail := domain.JobDetail{Job: job, Attempts: make([]domain.Attempt, 0), Metrics: make([]domain.RecordedMetric, 0), Artifacts: make([]domain.Artifact, 0)}
	attemptRows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, attempt_number, worker_id, state, revision, offered_at,
		       lease_expires_at, accepted_at, started_at, finished_at, exit_code, error, log_uri, log_tail
		FROM attempts WHERE job_id = ? ORDER BY attempt_number, id
	`, jobID)
	if err != nil {
		return domain.JobDetail{}, fmt.Errorf("list attempts: %w", err)
	}
	for attemptRows.Next() {
		var attempt domain.Attempt
		var offered, lease string
		var accepted, started, finished sql.NullString
		var exitCode sql.NullInt64
		if err := attemptRows.Scan(&attempt.ID, &attempt.JobID, &attempt.AttemptNumber, &attempt.WorkerID, &attempt.State, &attempt.Revision, &offered, &lease, &accepted, &started, &finished, &exitCode, &attempt.Error, &attempt.LogURI, &attempt.LogTail); err != nil {
			attemptRows.Close()
			return domain.JobDetail{}, err
		}
		if attempt.OfferedAt, err = time.Parse(time.RFC3339Nano, offered); err != nil {
			attemptRows.Close()
			return domain.JobDetail{}, err
		}
		if attempt.LeaseExpiresAt, err = time.Parse(time.RFC3339Nano, lease); err != nil {
			attemptRows.Close()
			return domain.JobDetail{}, err
		}
		if attempt.AcceptedAt, err = parseOptionalTime(accepted); err != nil {
			attemptRows.Close()
			return domain.JobDetail{}, err
		}
		if attempt.StartedAt, err = parseOptionalTime(started); err != nil {
			attemptRows.Close()
			return domain.JobDetail{}, err
		}
		if attempt.FinishedAt, err = parseOptionalTime(finished); err != nil {
			attemptRows.Close()
			return domain.JobDetail{}, err
		}
		if exitCode.Valid {
			value := int(exitCode.Int64)
			attempt.ExitCode = &value
		}
		detail.Attempts = append(detail.Attempts, attempt)
	}
	if err := attemptRows.Close(); err != nil {
		return domain.JobDetail{}, err
	}

	metricRows, err := s.db.QueryContext(ctx, `
		SELECT m.attempt_id, m.event_id, m.name, m.value, m.step, m.observed_at
		FROM metric_samples AS m JOIN attempts AS a ON a.id = m.attempt_id
		WHERE a.job_id = ? ORDER BY m.observed_at, m.event_id
	`, jobID)
	if err != nil {
		return domain.JobDetail{}, fmt.Errorf("list metrics: %w", err)
	}
	for metricRows.Next() {
		var metric domain.RecordedMetric
		var step sql.NullInt64
		var observed string
		if err := metricRows.Scan(&metric.AttemptID, &metric.EventID, &metric.Name, &metric.Value, &step, &observed); err != nil {
			metricRows.Close()
			return domain.JobDetail{}, err
		}
		if step.Valid {
			value := step.Int64
			metric.Step = &value
		}
		metric.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			metricRows.Close()
			return domain.JobDetail{}, err
		}
		detail.Metrics = append(detail.Metrics, metric)
	}
	if err := metricRows.Close(); err != nil {
		return domain.JobDetail{}, err
	}

	artifactRows, err := s.db.QueryContext(ctx, `
		SELECT f.attempt_id, f.name, f.uri, f.size_bytes, f.sha256, f.created_at
		FROM artifacts AS f JOIN attempts AS a ON a.id = f.attempt_id
		WHERE a.job_id = ? ORDER BY f.created_at, f.name, f.uri
	`, jobID)
	if err != nil {
		return domain.JobDetail{}, fmt.Errorf("list artifacts: %w", err)
	}
	for artifactRows.Next() {
		var artifact domain.Artifact
		var created string
		if err := artifactRows.Scan(&artifact.AttemptID, &artifact.Name, &artifact.URI, &artifact.SizeBytes, &artifact.SHA256, &created); err != nil {
			artifactRows.Close()
			return domain.JobDetail{}, err
		}
		artifact.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			artifactRows.Close()
			return domain.JobDetail{}, err
		}
		detail.Artifacts = append(detail.Artifacts, artifact)
	}
	if err := artifactRows.Close(); err != nil {
		return domain.JobDetail{}, err
	}
	return detail, nil
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
