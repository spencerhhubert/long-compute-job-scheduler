package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
)

const maxWebhookAttempts = 10

var targetNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type WebhookTarget struct {
	ID   string
	Name string
	URL  string
}

type WebhookDelivery struct {
	ID           string
	FiringID     string
	TargetName   string
	URL          string
	Secret       []byte
	Payload      []byte
	AttemptCount uint32
}

type healthCandidate struct {
	JobID         string
	AttemptID     string
	AttemptNumber uint32
	WorkerID      string
	StartedAt     time.Time
	Spec          domain.JobSpec
}

func (s *Store) CreateWebhookTarget(ctx context.Context, name, rawURL, secret string) (WebhookTarget, error) {
	name = strings.TrimSpace(name)
	if !targetNamePattern.MatchString(name) {
		return WebhookTarget{}, errors.New("webhook target name must be a lowercase slug of at most 63 characters")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return WebhookTarget{}, errors.New("webhook target URL must be an absolute URL without user information or a fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
		return WebhookTarget{}, errors.New("webhook target URL must use HTTPS outside loopback development")
	}
	if len(secret) < 32 {
		return WebhookTarget{}, errors.New("webhook signing secret must contain at least 32 characters")
	}
	targetID, err := id.New("hkt")
	if err != nil {
		return WebhookTarget{}, err
	}
	now := formatTime(s.now())
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_targets(id, name, url, secret, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, targetID, name, parsed.String(), []byte(secret), now); err != nil {
		return WebhookTarget{}, fmt.Errorf("create webhook target: %w", err)
	}
	return WebhookTarget{ID: targetID, Name: name, URL: parsed.String()}, nil
}

func (s *Store) EvaluateHealthPolicies(ctx context.Context) (int, error) {
	now := s.now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id, a.id, a.attempt_number, a.worker_id, a.started_at, j.spec_json
		FROM attempts AS a JOIN jobs AS j ON j.id = a.job_id
		WHERE a.state = ? AND j.state = ? AND a.started_at IS NOT NULL
		ORDER BY j.id, a.attempt_number
	`, domain.AttemptRunning, domain.JobRunning)
	if err != nil {
		return 0, fmt.Errorf("list health candidates: %w", err)
	}
	candidates := make([]healthCandidate, 0)
	for rows.Next() {
		var candidate healthCandidate
		var started string
		var specJSON []byte
		if err := rows.Scan(&candidate.JobID, &candidate.AttemptID, &candidate.AttemptNumber, &candidate.WorkerID, &started, &specJSON); err != nil {
			rows.Close()
			return 0, err
		}
		candidate.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal(specJSON, &candidate.Spec); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	fired := 0
	for _, candidate := range candidates {
		for index, policy := range candidate.Spec.Health {
			window, err := time.ParseDuration(policy.Window)
			if err != nil || window <= 0 {
				continue
			}
			var firingKey, reason string
			switch policy.Kind {
			case domain.HealthKindPeriodic:
				bucket := int64(now.Sub(candidate.StartedAt) / window)
				if bucket < 1 {
					continue
				}
				firingKey = fmt.Sprintf("periodic:%d", bucket)
				reason = fmt.Sprintf("attempt has been running for %s", now.Sub(candidate.StartedAt).Round(time.Second))
			case domain.HealthKindMetricStalled:
				if now.Sub(candidate.StartedAt) < window {
					continue
				}
				stalled, observedDelta, samples, err := s.metricStalled(ctx, candidate.AttemptID, policy, now.Add(-window))
				if err != nil {
					return fired, err
				}
				if !stalled {
					continue
				}
				firingKey = "stalled"
				reason = fmt.Sprintf("metric %s improved by %g across %d samples in %s; required at least %g", policy.Metric, observedDelta, samples, window, policy.MinimumDelta)
			default:
				continue
			}
			inserted, err := s.fireHealthPolicy(ctx, candidate, uint32(index), policy, firingKey, reason, now)
			if err != nil {
				return fired, err
			}
			if inserted {
				fired++
			}
		}
	}
	return fired, nil
}

func (s *Store) metricStalled(ctx context.Context, attemptID string, policy domain.HealthPolicy, cutoff time.Time) (bool, float64, int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT value, observed_at FROM metric_samples
		WHERE attempt_id = ? AND name = ?
	`, attemptID, policy.Metric)
	if err != nil {
		return false, 0, 0, fmt.Errorf("read health metric samples: %w", err)
	}
	defer rows.Close()
	type observedValue struct {
		value      float64
		observedAt time.Time
	}
	observations := make([]observedValue, 0)
	for rows.Next() {
		var value float64
		var observedRaw string
		if err := rows.Scan(&value, &observedRaw); err != nil {
			return false, 0, 0, err
		}
		observedAt, err := time.Parse(time.RFC3339Nano, observedRaw)
		if err != nil {
			return false, 0, 0, err
		}
		if observedAt.Before(cutoff) {
			continue
		}
		observations = append(observations, observedValue{value: value, observedAt: observedAt})
	}
	if err := rows.Err(); err != nil {
		return false, 0, 0, err
	}
	sort.Slice(observations, func(i, j int) bool { return observations[i].observedAt.Before(observations[j].observedAt) })
	if len(observations) < 2 {
		return false, 0, len(observations), nil
	}
	best := observations[0].value
	for _, observation := range observations[1:] {
		value := observation.value
		if policy.Mode == domain.HealthModeMax && value > best {
			best = value
		}
		if policy.Mode == domain.HealthModeMin && value < best {
			best = value
		}
	}
	delta := best - observations[0].value
	if policy.Mode == domain.HealthModeMin {
		delta = observations[0].value - best
	}
	return delta < policy.MinimumDelta, delta, len(observations), nil
}

func (s *Store) fireHealthPolicy(ctx context.Context, candidate healthCandidate, policyIndex uint32, policy domain.HealthPolicy, firingKey, reason string, now time.Time) (bool, error) {
	firingID, err := id.New("hlf")
	if err != nil {
		return false, err
	}
	deliveryID, err := id.New("dlv")
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(map[string]any{
		"event_id": firingID,
		"kind":     "health_policy_fired",
		"job": map[string]any{
			"id": candidate.JobID, "project": candidate.Spec.Project, "name": candidate.Spec.Name,
		},
		"attempt": map[string]any{
			"id": candidate.AttemptID, "number": candidate.AttemptNumber, "worker_id": candidate.WorkerID,
		},
		"policy_index": policyIndex,
		"policy":       policy,
		"reason":       reason,
		"observed_at":  now,
	})
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var targetID string
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM webhook_targets WHERE name = ? AND revoked_at IS NULL
	`, policy.Target).Scan(&targetID); errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("health target %q is not configured", policy.Target)
	} else if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO health_firings(id, job_id, attempt_id, policy_index, firing_key, kind, target_name, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, firingID, candidate.JobID, candidate.AttemptID, policyIndex, firingKey, policy.Kind, policy.Target, payload, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("record health firing: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO webhook_deliveries(id, firing_id, target_id, state, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)
	`, deliveryID, firingID, targetID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return false, fmt.Errorf("enqueue health webhook: %w", err)
	}
	if err := appendControlEvent(ctx, tx, "health_policy_fired", candidate.JobID, map[string]any{
		"firing_id": firingID, "attempt_id": candidate.AttemptID, "policy_index": policyIndex, "target": policy.Target,
	}, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) DueWebhookDeliveries(ctx context.Context, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.firing_id, t.name, t.url, t.secret, f.payload_json, d.attempt_count
		FROM webhook_deliveries AS d
		JOIN health_firings AS f ON f.id = d.firing_id
		JOIN webhook_targets AS t ON t.id = d.target_id
		WHERE d.state = 'pending' AND d.next_attempt_at <= ? AND t.revoked_at IS NULL
		ORDER BY d.next_attempt_at, d.created_at, d.id LIMIT ?
	`, formatTime(s.now()), limit)
	if err != nil {
		return nil, fmt.Errorf("list due webhook deliveries: %w", err)
	}
	defer rows.Close()
	deliveries := make([]WebhookDelivery, 0)
	for rows.Next() {
		var delivery WebhookDelivery
		if err := rows.Scan(&delivery.ID, &delivery.FiringID, &delivery.TargetName, &delivery.URL, &delivery.Secret, &delivery.Payload, &delivery.AttemptCount); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (s *Store) RecordWebhookResult(ctx context.Context, deliveryID string, responseCode int, deliveryError string) error {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts uint32
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_count, state FROM webhook_deliveries WHERE id = ?`, deliveryID).Scan(&attempts, &state); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if state != "pending" {
		return nil
	}
	attempts++
	if responseCode >= 200 && responseCode < 300 && deliveryError == "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE webhook_deliveries SET state = 'delivered', attempt_count = ?, response_code = ?, last_error = '', delivered_at = ?, updated_at = ? WHERE id = ?
		`, attempts, responseCode, formatTime(now), formatTime(now), deliveryID)
	} else {
		nextState := "pending"
		if attempts >= maxWebhookAttempts {
			nextState = "dead"
		}
		backoff := 5 * time.Second * time.Duration(1<<min(attempts-1, 10))
		if backoff > time.Hour {
			backoff = time.Hour
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE webhook_deliveries SET state = ?, attempt_count = ?, response_code = ?, last_error = ?, next_attempt_at = ?, updated_at = ? WHERE id = ?
		`, nextState, attempts, nullableResponseCode(responseCode), truncateString(deliveryError, 2000), formatTime(now.Add(backoff)), formatTime(now), deliveryID)
	}
	if err != nil {
		return fmt.Errorf("record webhook result: %w", err)
	}
	return tx.Commit()
}

func nullableResponseCode(code int) any {
	if code == 0 {
		return nil
	}
	return code
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
