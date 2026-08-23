package sqlite

import (
	"context"
	"errors"
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
		Capacity: domain.WorkerCapacity{CPU: 4, MemoryBytes: 8 << 30, MaxParallel: 1, Labels: map[string]string{"os": "linux"}},
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
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "event-accepted", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now.Add(time.Second)},
		{Sequence: 2, EventID: "event-started", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now.Add(2 * time.Second)},
		{Sequence: 3, EventID: "event-metric", AttemptID: command.AttemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now.Add(3 * time.Second), Metric: &domain.MetricSample{Name: "validation/accuracy", Value: 0.91, Step: &step, ObservedAt: now.Add(3 * time.Second)}},
		{Sequence: 4, EventID: "event-finished", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now.Add(4 * time.Second), ExitCode: &exitCode, LogURI: "worker://worker-1/job/attempt/log", Artifacts: []domain.ArtifactAnnouncement{{Name: "result", URI: "worker://worker-1/job/attempt/result", SizeBytes: 12, SHA256: "abc"}}},
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
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatalf("idempotent event replay: %v", err)
	}
	for table, want := range map[string]int{"worker_events": 4, "metric_samples": 1, "artifacts": 1} {
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

func TestWorkerSchedulingHonorsLabelsAndParallelCapacity(t *testing.T) {
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
	response, err := store.SyncWorker(ctx, credential.WorkerID, domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 8, MaxParallel: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(response.Commands))
	}
}
