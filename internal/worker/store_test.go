package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func TestStorePersistsCommandsAndOutboxAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "worker.db")
	store, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 23, 2, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	command := domain.WorkerCommand{
		CommandID: "command-1", AttemptID: "attempt-1", AttemptNumber: 1, Kind: "run_job",
		Job: domain.Job{ID: "job-1", Spec: domain.JobSpec{Project: "project", Name: "job"}},
	}
	if err := store.AcceptCommands(ctx, []domain.WorkerCommand{command, command}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != 1 || events[0].Kind != domain.WorkerEventAttemptAccepted {
		t.Fatalf("accepted events = %+v", events)
	}
	gitState := &domain.GitState{Commit: "0123456789abcdef0123456789abcdef01234567", Branch: "main"}
	if err := store.MarkStarted(ctx, command.CommandID, 123, "/work", "/status", "/metrics", gitState); err != nil {
		t.Fatal(err)
	}
	step := int64(4)
	if err := store.EnqueueMetric(ctx, command.CommandID, command.AttemptID, domain.MetricSample{Name: "accuracy", Value: 0.9, Step: &step, ObservedAt: now}, 42); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	running, err := reopened.RunningCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].PID != 123 || running[0].MetricOffset != 42 {
		t.Fatalf("running commands = %+v", running)
	}
	events, err = reopened.Events(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Kind != domain.WorkerEventMetricSample {
		t.Fatalf("persisted events = %+v", events)
	}
	if events[1].Kind != domain.WorkerEventAttemptStarted || events[1].Git == nil || *events[1].Git != *gitState {
		t.Fatalf("started event = %+v, want recorded git state %+v", events[1], gitState)
	}
	if err := reopened.Acknowledge(ctx, 2); err != nil {
		t.Fatal(err)
	}
	events, err = reopened.Events(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != 3 {
		t.Fatalf("events after acknowledgement = %+v", events)
	}
	if err := reopened.MarkFinished(ctx, command.CommandID, 0, "", "worker://worker/log", "finished cleanly", nil); err != nil {
		t.Fatal(err)
	}
	if commands, err := reopened.RunningCommands(ctx); err != nil || len(commands) != 0 {
		t.Fatalf("running after finish = %+v, %v", commands, err)
	}
}
