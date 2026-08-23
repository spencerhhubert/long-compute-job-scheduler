package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func TestListJobMetricPointsUsesIncrementalCursor(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 23, 1, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	credential, err := store.CreateWorker(ctx, "worker-1", "lcjw_0123456789abcdef0123456789abcdef0123456789abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateJob(ctx, "metric-cursor-job", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "session-1", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offered, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered.Commands) != 1 {
		t.Fatalf("commands = %+v, want one offer", offered.Commands)
	}
	attemptID := offered.Commands[0].AttemptID

	step := int64(10)
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "event-started", AttemptID: attemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now},
		{Sequence: 2, EventID: "event-metric-1", AttemptID: attemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now,
			Metric: &domain.MetricSample{Name: "validation/accuracy", Value: 0.91, Step: &step, ObservedAt: now.Add(time.Second)}},
		{Sequence: 3, EventID: "event-metric-2", AttemptID: attemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now,
			Metric: &domain.MetricSample{Name: domain.SystemMetricPrefix + "rss_bytes", Value: 2 << 30, ObservedAt: now.Add(2 * time.Second)}},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}

	points, cursor, err := store.ListJobMetricPoints(ctx, created.Job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || cursor <= 0 {
		t.Fatalf("points = %+v, cursor = %d, want 2 points and a positive cursor", points, cursor)
	}
	if points[0].Name != "validation/accuracy" || points[0].Value != 0.91 || points[0].Attempt != 1 ||
		points[0].Step == nil || *points[0].Step != step || !points[0].ObservedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("first point = %+v", points[0])
	}
	if points[1].Name != domain.SystemMetricPrefix+"rss_bytes" || points[1].Step != nil {
		t.Fatalf("second point = %+v, want automatic series without a step", points[1])
	}

	repeat, repeatCursor, err := store.ListJobMetricPoints(ctx, created.Job.ID, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeat) != 0 || repeatCursor != cursor {
		t.Fatalf("points after cursor = %+v, cursor = %d, want none and an unchanged cursor", repeat, repeatCursor)
	}

	request.Events = []domain.WorkerEvent{
		{Sequence: 4, EventID: "event-metric-3", AttemptID: attemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now,
			Metric: &domain.MetricSample{Name: "validation/accuracy", Value: 0.93, ObservedAt: now.Add(3 * time.Second)}},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	fresh, freshCursor, err := store.ListJobMetricPoints(ctx, created.Job.ID, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].Value != 0.93 || freshCursor <= cursor {
		t.Fatalf("incremental points = %+v, cursor = %d, want only the new sample", fresh, freshCursor)
	}

	none, _, err := store.ListJobMetricPoints(ctx, "job_missing", 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown job points = %+v, err = %v, want empty", none, err)
	}
}
