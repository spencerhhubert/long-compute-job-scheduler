package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
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

func TestCancelRunningJobDeliversCancelCommandAndLandsCanceled(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 24, 1, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	credential, err := store.CreateWorker(ctx, "cancel-worker", "lcjw_cancel0123456789abcdef0123456789abcdef012345678", nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	// Two attempts allowed: cancellation must still not schedule a retry.
	spec.Retry.MaxAttempts = 2
	created, err := store.CreateJob(ctx, "cancel-running-job", spec)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "cancel-session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offer, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil || len(offer.Commands) != 1 {
		t.Fatalf("offer = %+v, %v", offer, err)
	}
	command := offer.Commands[0]
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "cancel-accepted", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now},
		{Sequence: 2, EventID: "cancel-started", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	running, err := store.GetJob(ctx, created.Job.ID)
	if err != nil || running.State != domain.JobRunning {
		t.Fatalf("job = %+v, %v", running, err)
	}

	canceling, err := store.CancelJob(ctx, created.Job.ID)
	if err != nil || canceling.State != domain.JobCanceling {
		t.Fatalf("cancel = %+v, %v", canceling, err)
	}
	repeated, err := store.CancelJob(ctx, created.Job.ID)
	if err != nil || repeated.State != domain.JobCanceling {
		t.Fatalf("repeated cancel = %+v, %v", repeated, err)
	}

	request.Events = nil
	synced, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced.Commands) != 1 {
		t.Fatalf("sync commands = %+v", synced.Commands)
	}
	cancel := synced.Commands[0]
	if cancel.Kind != domain.WorkerCommandCancelAttempt || cancel.AttemptID != command.AttemptID || cancel.CommandID != command.CommandID+"-cancel" {
		t.Fatalf("cancel command = %+v", cancel)
	}

	exitCode := -1
	request.Events = []domain.WorkerEvent{{
		Sequence: 3, EventID: "cancel-finished", AttemptID: command.AttemptID,
		Kind: domain.WorkerEventAttemptFinished, OccurredAt: now,
		ExitCode: &exitCode, Error: "signal: terminated",
	}}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetJobDetail(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Job.State != domain.JobCanceled {
		t.Fatalf("job after cancel = %+v", detail.Job)
	}
	if len(detail.Attempts) != 1 || detail.Attempts[0].State != domain.AttemptCanceled {
		t.Fatalf("attempts = %+v", detail.Attempts)
	}
	request.Events = nil
	after, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, remaining := range after.Commands {
		if remaining.Kind == domain.WorkerCommandCancelAttempt {
			t.Fatalf("cancel command still delivered after terminal attempt: %+v", remaining)
		}
	}
}

func TestCancelQueuedJobWithOfferedAttemptWaitsForWorker(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 24, 2, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	credential, err := store.CreateWorker(ctx, "offered-worker", "lcjw_offered0123456789abcdef0123456789abcdef01234567", nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateJob(ctx, "cancel-offered-job", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "offered-session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offer, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil || len(offer.Commands) != 1 {
		t.Fatalf("offer = %+v, %v", offer, err)
	}
	command := offer.Commands[0]

	// The attempt is offered, so the worker may already hold it: the job must
	// wait in canceling until the worker reports the attempt finished.
	canceling, err := store.CancelJob(ctx, created.Job.ID)
	if err != nil || canceling.State != domain.JobCanceling {
		t.Fatalf("cancel = %+v, %v", canceling, err)
	}
	synced, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	sawCancel := false
	for _, delivered := range synced.Commands {
		if delivered.Kind == domain.WorkerCommandCancelAttempt && delivered.AttemptID == command.AttemptID {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatalf("no cancel command for offered attempt: %+v", synced.Commands)
	}
	exitCode := -1
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "offered-accepted", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now},
		{Sequence: 2, EventID: "offered-finished", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now, ExitCode: &exitCode, Error: "canceled before start"},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	final, err := store.GetJob(ctx, created.Job.ID)
	if err != nil || final.State != domain.JobCanceled {
		t.Fatalf("final job = %+v, %v", final, err)
	}
}

func TestFinishedHealthPolicyFiresWebhookOnAttemptFinished(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 24, 3, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	const secret = "lcjh_finished9abcdef0123456789abcdef0123456789abc"
	if _, err := store.CreateWebhookTarget(ctx, "agent-session", "http://127.0.0.1/lcjs", secret); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Health = []domain.HealthPolicy{{Kind: domain.HealthKindFinished, Action: domain.HealthActionNotify, Target: "agent-session"}}
	created, err := store.CreateJob(ctx, "finished-hook-job", spec)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := store.CreateWorker(ctx, "finished-worker", "lcjw_finished123456789abcdef0123456789abcdef0123456", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "finished-session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offer, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil || len(offer.Commands) != 1 {
		t.Fatalf("offer = %+v, %v", offer, err)
	}
	command := offer.Commands[0]
	exitCode := 0
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "finished-accepted", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now},
		{Sequence: 2, EventID: "finished-started", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now},
		{Sequence: 3, EventID: "finished-finished", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now, ExitCode: &exitCode},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.DueWebhookDeliveries(ctx, 20)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v, %v", deliveries, err)
	}
	payload := string(deliveries[0].Payload)
	for _, want := range []string{created.Job.ID, `"attempt_state":"succeeded"`, `"exit_code":0`, `"finished"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("payload %s does not contain %s", payload, want)
		}
	}
	// Replaying the same worker event must not create a second firing.
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	var firings int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM health_firings").Scan(&firings); err != nil {
		t.Fatal(err)
	}
	if firings != 1 {
		t.Fatalf("firings = %d, want 1", firings)
	}
}
