// Package worker implements the durable outbound worker agent.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type StoredCommand struct {
	Command      domain.WorkerCommand
	State        string
	PID          int
	WorkDir      string
	StatusPath   string
	MetricsPath  string
	MetricOffset int64
	// CancelRequestedAt is zero until the control plane asks for this attempt
	// to stop. It times the escalation from SIGTERM to SIGKILL.
	CancelRequestedAt time.Time
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("worker database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create worker database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open worker sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, now: time.Now}
	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("set worker database permissions: %w", err)
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		`CREATE TABLE IF NOT EXISTS commands (
			command_id     TEXT PRIMARY KEY,
			attempt_id     TEXT NOT NULL UNIQUE,
			command_json   BLOB NOT NULL CHECK (json_valid(command_json)),
			state          TEXT NOT NULL,
			pid            INTEGER NOT NULL DEFAULT 0,
			work_dir       TEXT NOT NULL DEFAULT '',
			status_path    TEXT NOT NULL DEFAULT '',
			metrics_path   TEXT NOT NULL DEFAULT '',
			metric_offset  INTEGER NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL,
			updated_at     TEXT NOT NULL
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS outbox (
			sequence     INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id     TEXT NOT NULL UNIQUE,
			event_json   BLOB NOT NULL CHECK (json_valid(event_json)),
			created_at   TEXT NOT NULL
		) STRICT`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize worker store: %w", err)
		}
	}
	// The worker store predates cancellation; add the column to existing
	// databases and ignore the error once it is present.
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE commands ADD COLUMN cancel_requested_at TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add cancel_requested_at column: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) AcceptCommands(ctx context.Context, commands []domain.WorkerCommand) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin accept commands: %w", err)
	}
	defer tx.Rollback()
	for _, command := range commands {
		now := s.now().UTC()
		switch command.Kind {
		case domain.WorkerCommandRunJob:
		case domain.WorkerCommandCancelAttempt:
			// Mark the run command this cancel refers to; the executor stops
			// the attempt. Repeats and cancels for attempts already finished
			// or never held are no-ops.
			if _, err := tx.ExecContext(ctx, `
				UPDATE commands SET cancel_requested_at = ?, updated_at = ?
				WHERE attempt_id = ? AND state IN ('pending', 'running') AND cancel_requested_at = ''
			`, formatTime(now), formatTime(now), command.AttemptID); err != nil {
				return fmt.Errorf("record cancel request: %w", err)
			}
			continue
		default:
			// A newer control plane may send kinds this worker does not know;
			// skipping them keeps the sync loop alive.
			slog.Warn("ignoring unknown worker command kind", "kind", command.Kind, "command_id", command.CommandID)
			continue
		}
		encoded, err := json.Marshal(command)
		if err != nil {
			return fmt.Errorf("encode worker command: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO commands(command_id, attempt_id, command_json, state, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', ?, ?)
		`, command.CommandID, command.AttemptID, encoded, formatTime(now), formatTime(now))
		if err != nil {
			return fmt.Errorf("store worker command: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			var existing []byte
			if err := tx.QueryRowContext(ctx, `SELECT command_json FROM commands WHERE command_id = ?`, command.CommandID).Scan(&existing); err != nil {
				return fmt.Errorf("read duplicate command: %w", err)
			}
			if string(existing) != string(encoded) {
				return fmt.Errorf("command %s changed after it was accepted", command.CommandID)
			}
			continue
		}
		if err := enqueueEvent(ctx, tx, domain.WorkerEvent{
			AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now,
		}, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit accepted commands: %w", err)
	}
	return nil
}

// PendingCommands lists commands ready to launch; a pending command whose
// cancellation was requested is never launched and is finished by
// reconcileRunning instead.
func (s *Store) PendingCommands(ctx context.Context, limit int) ([]StoredCommand, error) {
	return s.listCommands(ctx, `state = 'pending' AND cancel_requested_at = ''`, limit)
}

func (s *Store) PendingCanceledCommands(ctx context.Context) ([]StoredCommand, error) {
	return s.listCommands(ctx, `state = 'pending' AND cancel_requested_at != ''`, 1000)
}

func (s *Store) RunningCommands(ctx context.Context) ([]StoredCommand, error) {
	return s.listCommands(ctx, `state = 'running'`, 1000)
}

func (s *Store) listCommands(ctx context.Context, where string, limit int) ([]StoredCommand, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT command_json, state, pid, work_dir, status_path, metrics_path, metric_offset, cancel_requested_at
		FROM commands WHERE `+where+` ORDER BY created_at, command_id LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list commands where %s: %w", where, err)
	}
	defer rows.Close()
	commands := make([]StoredCommand, 0)
	for rows.Next() {
		var command StoredCommand
		var encoded []byte
		var cancelRequested string
		if err := rows.Scan(&encoded, &command.State, &command.PID, &command.WorkDir, &command.StatusPath, &command.MetricsPath, &command.MetricOffset, &cancelRequested); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &command.Command); err != nil {
			return nil, fmt.Errorf("decode stored command: %w", err)
		}
		if cancelRequested != "" {
			parsed, err := time.Parse(time.RFC3339Nano, cancelRequested)
			if err != nil {
				return nil, fmt.Errorf("parse cancel request time: %w", err)
			}
			command.CancelRequestedAt = parsed
		}
		commands = append(commands, command)
	}
	return commands, rows.Err()
}

func (s *Store) MarkStarted(ctx context.Context, commandID string, pid int, workDir, statusPath, metricsPath string, gitState *domain.GitState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE commands SET state = 'running', pid = ?, work_dir = ?, status_path = ?, metrics_path = ?, updated_at = ?
		WHERE command_id = ? AND state = 'pending'
	`, pid, workDir, statusPath, metricsPath, formatTime(now), commandID)
	if err != nil {
		return fmt.Errorf("mark command started: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("command %s is not pending", commandID)
	}
	var attemptID string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM commands WHERE command_id = ?`, commandID).Scan(&attemptID); err != nil {
		return err
	}
	if err := enqueueEvent(ctx, tx, domain.WorkerEvent{AttemptID: attemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now, Git: gitState}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkFinished(ctx context.Context, commandID string, exitCode int, runError, logURI, logTail string, artifacts []domain.ArtifactAnnouncement) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	// A running command finishes when its process ends; a pending one
	// finishes directly only when it was canceled before launch.
	result, err := tx.ExecContext(ctx, `UPDATE commands SET state = 'finished', updated_at = ? WHERE command_id = ? AND state IN ('running', 'pending')`, formatTime(now), commandID)
	if err != nil {
		return fmt.Errorf("mark command finished: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("command %s is not running or pending", commandID)
	}
	var attemptID string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM commands WHERE command_id = ?`, commandID).Scan(&attemptID); err != nil {
		return err
	}
	if err := enqueueEvent(ctx, tx, domain.WorkerEvent{
		AttemptID: attemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now,
		ExitCode: &exitCode, Error: runError, LogURI: logURI, LogTail: logTail, Artifacts: artifacts,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AnnounceArtifacts records output produced so far without ending the
// attempt, so a long run's results can be read while it is still going.
func (s *Store) AnnounceArtifacts(ctx context.Context, commandID string, artifacts []domain.ArtifactAnnouncement) error {
	if len(artifacts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	var attemptID string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM commands WHERE command_id = ? AND state = 'running'`, commandID).Scan(&attemptID); err != nil {
		return err
	}
	if err := enqueueEvent(ctx, tx, domain.WorkerEvent{
		AttemptID: attemptID, Kind: domain.WorkerEventArtifacts,
		OccurredAt: now, Artifacts: artifacts,
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

// SkipMetric advances past a line the worker refuses to send without
// enqueuing anything, so a job cannot wedge the cycle with one bad value.
func (s *Store) SkipMetric(ctx context.Context, commandID string, newOffset int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE commands SET metric_offset = ?, updated_at = ? WHERE command_id = ?`,
		newOffset, formatTime(s.now().UTC()), commandID)
	return err
}

func (s *Store) EnqueueMetric(ctx context.Context, commandID, attemptID string, sample domain.MetricSample, newOffset int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if err := enqueueEvent(ctx, tx, domain.WorkerEvent{
		AttemptID: attemptID, Kind: domain.WorkerEventMetricSample,
		OccurredAt: sample.ObservedAt, Metric: &sample,
	}, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET metric_offset = ?, updated_at = ? WHERE command_id = ?`, newOffset, formatTime(now), commandID); err != nil {
		return fmt.Errorf("advance metric offset: %w", err)
	}
	return tx.Commit()
}

func enqueueEvent(ctx context.Context, tx *sql.Tx, event domain.WorkerEvent, now time.Time) error {
	eventID, err := id.New("wev")
	if err != nil {
		return err
	}
	event.EventID = eventID
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(event_id, event_json, created_at) VALUES (?, ?, ?)`, eventID, encoded, formatTime(now)); err != nil {
		return fmt.Errorf("enqueue worker event: %w", err)
	}
	return nil
}

func (s *Store) Events(ctx context.Context, limit int) ([]domain.WorkerEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, event_json FROM outbox ORDER BY sequence LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read worker outbox: %w", err)
	}
	defer rows.Close()
	events := make([]domain.WorkerEvent, 0)
	for rows.Next() {
		var event domain.WorkerEvent
		var encoded []byte
		if err := rows.Scan(&event.Sequence, &encoded); err != nil {
			return nil, err
		}
		sequence := event.Sequence
		if err := json.Unmarshal(encoded, &event); err != nil {
			return nil, fmt.Errorf("decode worker event: %w", err)
		}
		event.Sequence = sequence
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) Acknowledge(ctx context.Context, through uint64) error {
	if through == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE sequence <= ?`, through); err != nil {
		return fmt.Errorf("acknowledge worker outbox: %w", err)
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
