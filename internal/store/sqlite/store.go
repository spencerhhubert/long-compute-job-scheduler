// Package sqlite implements durable control-plane storage in SQLite.
package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrInvalidTransition   = errors.New("invalid state transition")
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type CreateJobResult struct {
	Job     domain.Job
	Created bool
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if path != ":memory:" {
		directory := filepath.Dir(path)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, now: time.Now}
	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("set database permissions: %w", err)
		}
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite with %q: %w", statement, err)
		}
	}

	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := s.applyMigration(ctx, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name string) error {
	var exists int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM schema_migrations WHERE name = ?", name,
	).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check migration %s: %w", name, err)
	}

	script, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, string(script)); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(name, applied_at) VALUES (?, ?)",
		name, formatTime(s.now()),
	); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CreateJob(ctx context.Context, key string, spec domain.JobSpec) (CreateJobResult, error) {
	if key == "" {
		return CreateJobResult{}, errors.New("idempotency key is required")
	}
	if err := spec.Validate(); err != nil {
		return CreateJobResult{}, fmt.Errorf("validate job: %w", err)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return CreateJobResult{}, fmt.Errorf("encode job spec: %w", err)
	}
	requestHash := sha256.Sum256(specJSON)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateJobResult{}, fmt.Errorf("begin create job: %w", err)
	}
	defer tx.Rollback()

	var storedHash []byte
	var existingID string
	err = tx.QueryRowContext(ctx, `
		SELECT request_hash, resource_id
		FROM idempotency_keys
		WHERE operation = 'create_job' AND key = ?
	`, key).Scan(&storedHash, &existingID)
	if err == nil {
		if !bytes.Equal(storedHash, requestHash[:]) {
			return CreateJobResult{}, ErrIdempotencyConflict
		}
		job, err := getJob(ctx, tx, existingID)
		if err != nil {
			return CreateJobResult{}, err
		}
		return CreateJobResult{Job: job, Created: false}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CreateJobResult{}, fmt.Errorf("read idempotency key: %w", err)
	}
	checkedTargets := make(map[string]struct{})
	for _, policy := range spec.Health {
		if _, checked := checkedTargets[policy.Target]; checked {
			continue
		}
		var exists int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM webhook_targets WHERE name = ? AND revoked_at IS NULL
		`, policy.Target).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return CreateJobResult{}, fmt.Errorf("validate job: health target %q is not configured", policy.Target)
		}
		if err != nil {
			return CreateJobResult{}, fmt.Errorf("validate job health target: %w", err)
		}
		checkedTargets[policy.Target] = struct{}{}
	}

	jobID, err := id.New("job")
	if err != nil {
		return CreateJobResult{}, err
	}
	eventID, err := id.New("evt")
	if err != nil {
		return CreateJobResult{}, err
	}
	now := s.now().UTC()
	job := domain.Job{
		ID: jobID, State: domain.JobQueued, Revision: 1, Spec: spec,
		CreatedAt: now, UpdatedAt: now,
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(id, project, name, state, priority, revision, spec_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, job.ID, spec.Project, spec.Name, job.State, spec.Priority, job.Revision,
		specJSON, formatTime(now), formatTime(now)); err != nil {
		return CreateJobResult{}, fmt.Errorf("insert job: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"job_id": job.ID, "revision": job.Revision})
	if err != nil {
		return CreateJobResult{}, fmt.Errorf("encode job event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events(event_id, kind, resource_id, payload_json, occurred_at, recorded_at)
		VALUES (?, 'job_submitted', ?, ?, ?, ?)
	`, eventID, job.ID, payload, formatTime(now), formatTime(now)); err != nil {
		return CreateJobResult{}, fmt.Errorf("insert job event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO idempotency_keys(operation, key, request_hash, resource_id, created_at)
		VALUES ('create_job', ?, ?, ?, ?)
	`, key, requestHash[:], job.ID, formatTime(now)); err != nil {
		return CreateJobResult{}, fmt.Errorf("insert idempotency key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CreateJobResult{}, fmt.Errorf("commit create job: %w", err)
	}
	return CreateJobResult{Job: job, Created: true}, nil
}

func (s *Store) GetJob(ctx context.Context, jobID string) (domain.Job, error) {
	return getJob(ctx, s.db, jobID)
}

func (s *Store) CancelJob(ctx context.Context, jobID string) (domain.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, fmt.Errorf("begin cancel job: %w", err)
	}
	defer tx.Rollback()
	job, err := getJob(ctx, tx, jobID)
	if err != nil {
		return domain.Job{}, err
	}
	if job.State == domain.JobCanceled {
		return job, nil
	}
	if job.State != domain.JobQueued && job.State != domain.JobRetryWait {
		return domain.Job{}, fmt.Errorf("%w: cannot cancel job in state %s", ErrInvalidTransition, job.State)
	}
	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, revision = revision + 1, updated_at = ?
		WHERE id = ? AND revision = ? AND state IN (?, ?)
	`, domain.JobCanceled, formatTime(now), job.ID, job.Revision, domain.JobQueued, domain.JobRetryWait)
	if err != nil {
		return domain.Job{}, fmt.Errorf("cancel job: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return domain.Job{}, fmt.Errorf("%w: job changed while canceling", ErrInvalidTransition)
	}
	if err := appendControlEvent(ctx, tx, "job_canceled", job.ID, map[string]any{"job_id": job.ID}, now); err != nil {
		return domain.Job{}, fmt.Errorf("record cancellation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Job{}, fmt.Errorf("commit cancellation: %w", err)
	}
	return s.GetJob(ctx, job.ID)
}

func (s *Store) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, state, revision, spec_json, created_at, updated_at
		FROM jobs
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]domain.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list job rows: %w", err)
	}
	return jobs, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

type jobQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getJob(ctx context.Context, queryer jobQuerier, jobID string) (domain.Job, error) {
	job, err := scanJob(queryer.QueryRowContext(ctx, `
		SELECT id, state, revision, spec_json, created_at, updated_at
		FROM jobs WHERE id = ?
	`, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	return job, err
}

func scanJob(row rowScanner) (domain.Job, error) {
	var job domain.Job
	var specJSON []byte
	var createdAt, updatedAt string
	if err := row.Scan(&job.ID, &job.State, &job.Revision, &specJSON, &createdAt, &updatedAt); err != nil {
		return domain.Job{}, err
	}
	if err := json.Unmarshal(specJSON, &job.Spec); err != nil {
		return domain.Job{}, fmt.Errorf("decode stored job %s: %w", job.ID, err)
	}
	var err error
	job.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.Job{}, fmt.Errorf("parse created time for job %s: %w", job.ID, err)
	}
	job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return domain.Job{}, fmt.Errorf("parse updated time for job %s: %w", job.ID, err)
	}
	return job, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
