package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func TestWorkerSyncDispatchesAndCompletesJobIdempotently(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 23, 1, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	const rawToken = "lcjw_0123456789abcdef0123456789abcdef0123456789abc"
	credential, err := store.CreateWorker(ctx, "worker-1", rawToken, map[string]string{"os": "linux"})
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := store.AuthenticateWorkerToken(ctx, rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.WorkerID != credential.WorkerID {
		t.Fatalf("authenticated worker = %+v, want %s", authenticated, credential.WorkerID)
	}
	if _, err := store.AuthenticateWorkerToken(ctx, "invalid-token-that-is-long-enough-for-auth"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid token error = %v, want ErrNotFound", err)
	}
	created, err := store.CreateJob(ctx, "worker-sync-job", testSpec())
	if err != nil {
		t.Fatal(err)
	}

	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "session-1", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MemoryBytes: 8 << 30, MaxParallel: 1, Labels: map[string]string{"os": "linux"}, Projects: []string{"example-research"}},
	}
	first, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Commands) != 1 || first.Commands[0].Job.ID != created.Job.ID {
		t.Fatalf("first sync commands = %+v", first.Commands)
	}
	command := first.Commands[0]
	repeated, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Commands) != 1 || repeated.Commands[0].CommandID != command.CommandID {
		t.Fatalf("repeated offer = %+v, want command %s", repeated.Commands, command.CommandID)
	}

	exitCode := 0
	step := int64(10)
	gitState := &domain.GitState{Commit: "0123456789abcdef0123456789abcdef01234567", Branch: "main", Dirty: true}
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "event-accepted", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now.Add(time.Second)},
		{Sequence: 2, EventID: "event-started", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now.Add(2 * time.Second), Git: gitState},
		{Sequence: 3, EventID: "event-metric", AttemptID: command.AttemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now.Add(3 * time.Second), Metric: &domain.MetricSample{Name: "validation/accuracy", Value: 0.91, Step: &step, ObservedAt: now.Add(3 * time.Second)}},
		{Sequence: 4, EventID: "event-finished", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now.Add(4 * time.Second), ExitCode: &exitCode, LogURI: "worker://worker-1/job/attempt/log", LogTail: "training complete\n", Artifacts: []domain.ArtifactAnnouncement{
			{Name: "result", URI: "worker://worker-1/job/attempt/result/champion.json", SizeBytes: 16, SHA256: "abc", Content: `{"score": 0.91}` + "\n"},
			{Name: "checkpoint", URI: "worker://worker-1/job/attempt/checkpoint/model.pt", SizeBytes: 1 << 30, SHA256: "def"},
		}},
	}
	finished, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if finished.AcceptedThrough != 4 || len(finished.Commands) != 0 {
		t.Fatalf("finished sync = %+v", finished)
	}
	job, err := store.GetJob(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.JobSucceeded {
		t.Fatalf("job state = %s, want succeeded", job.State)
	}
	detail, err := store.GetJobDetail(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Attempts) != 1 || detail.Attempts[0].State != domain.AttemptSucceeded {
		t.Fatalf("attempts = %+v, want one succeeded attempt", detail.Attempts)
	}
	if detail.Attempts[0].LogTail != "training complete\n" {
		t.Fatalf("log tail = %q", detail.Attempts[0].LogTail)
	}
	if got := detail.Attempts[0].Git; got == nil || *got != *gitState {
		t.Fatalf("recorded git state = %+v, want %+v", got, gitState)
	}
	if len(detail.Metrics) != 1 || detail.Metrics[0].Name != "validation/accuracy" || detail.Metrics[0].Value != 0.91 {
		t.Fatalf("metrics = %+v, want recorded accuracy", detail.Metrics)
	}
	if len(detail.Artifacts) != 2 || detail.Artifacts[1].Name != "result" || detail.Artifacts[1].SHA256 != "abc" {
		t.Fatalf("artifacts = %+v, want recorded result", detail.Artifacts)
	}
	if detail.Artifacts[1].Content != `{"score": 0.91}`+"\n" {
		t.Fatalf("artifact content = %q, want the announced text round-tripped", detail.Artifacts[1].Content)
	}
	if detail.Artifacts[0].Name != "checkpoint" || detail.Artifacts[0].Content != "" {
		t.Fatalf("checkpoint artifact = %+v, want metadata without content", detail.Artifacts[0])
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatalf("idempotent event replay: %v", err)
	}
	for table, want := range map[string]int{"worker_events": 4, "metric_samples": 1, "artifacts": 2} {
		var got int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
	workers, err := store.ListWorkers(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Status != domain.WorkerOnline {
		t.Fatalf("workers = %+v", workers)
	}
}

func TestWorkerSchedulingHonorsProjectsLabelsAndParallelCapacity(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	credential, err := store.CreateWorker(ctx, "cpu-worker", "lcjw_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		spec := testSpec()
		if index == 2 {
			spec.Constraints.Labels = map[string]string{"gpu": "true"}
		}
		if _, err := store.CreateJob(ctx, "parallel-"+string(rune('a'+index)), spec); err != nil {
			t.Fatal(err)
		}
	}
	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 8, MaxParallel: 2, Projects: []string{"another-project"}},
	}
	unmapped, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(unmapped.Commands) != 0 {
		t.Fatalf("commands for a worker without the project = %d, want 0", len(unmapped.Commands))
	}
	request.Capacity.Projects = []string{"another-project", "example-research"}
	response, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(response.Commands))
	}
}

// TestArtifactContentMigrationAppliesOverExistingRows seeds artifact rows in
// the schema as it was before the content column, then reopens the database so
// Open applies the remaining migrations over them.
func TestArtifactContentMigrationAppliesOverExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Store{db: db, now: time.Now}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0008" {
			continue
		}
		if err := legacy.applyMigration(ctx, entry.Name()); err != nil {
			t.Fatal(err)
		}
	}
	created, err := legacy.CreateJob(ctx, "legacy-artifact-job", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := legacy.CreateWorker(ctx, "worker-legacy", "lcjw_legacy0123456789abcdef0123456789abcdef01234", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := db.ExecContext(ctx, `
		INSERT INTO attempts(id, job_id, attempt_number, worker_id, command_id, state, revision, offered_at, lease_expires_at)
		VALUES ('att_legacy', ?, 1, ?, 'cmd_legacy', 'succeeded', 1, ?, ?)
	`, created.Job.ID, credential.WorkerID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artifacts(attempt_id, name, uri, size_bytes, sha256, created_at)
		VALUES ('att_legacy', 'result', 'worker://worker-legacy/job/1/result', 12, 'abc', ?)
	`, now); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening applies the content migration: %v", err)
	}
	defer store.Close()
	detail, err := store.GetJobDetail(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Name != "result" || detail.Artifacts[0].Content != "" {
		t.Fatalf("migrated artifacts = %+v, want the seeded row without content", detail.Artifacts)
	}
}

func TestWorkerSyncNewSessionRestartsEventSequences(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 23, 2, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	credential, err := store.CreateWorker(ctx, "worker-1", "lcjw_0123456789abcdef0123456789abcdef0123456789abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func(session, jobKey, eventPrefix string) {
		created, err := store.CreateJob(ctx, jobKey, testSpec())
		if err != nil {
			t.Fatal(err)
		}
		request := domain.WorkerSyncRequest{
			WorkerID: credential.WorkerID, SessionID: session, AgentVersion: "test",
			Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
		}
		offered, err := store.SyncWorker(ctx, credential.WorkerID, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(offered.Commands) != 1 || offered.Commands[0].Job.ID != created.Job.ID {
			t.Fatalf("session %s offer = %+v", session, offered.Commands)
		}
		exitCode := 0
		attemptID := offered.Commands[0].AttemptID
		// A fresh agent session restarts its durable outbox at sequence 1.
		request.Events = []domain.WorkerEvent{
			{Sequence: 1, EventID: eventPrefix + "-accepted", AttemptID: attemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now},
			{Sequence: 2, EventID: eventPrefix + "-started", AttemptID: attemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now},
			{Sequence: 3, EventID: eventPrefix + "-finished", AttemptID: attemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now, ExitCode: &exitCode},
		}
		finished, err := store.SyncWorker(ctx, credential.WorkerID, request)
		if err != nil {
			t.Fatalf("session %s events: %v", session, err)
		}
		if finished.AcceptedThrough != 3 {
			t.Fatalf("session %s accepted through = %d, want 3", session, finished.AcceptedThrough)
		}
	}
	run("session-1", "seq-job-1", "one")
	run("session-2", "seq-job-2", "two")
}
