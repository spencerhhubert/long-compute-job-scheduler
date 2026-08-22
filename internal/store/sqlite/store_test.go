package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func testSpec() domain.JobSpec {
	return domain.JobSpec{
		Project: "example-research",
		Name:    "train-baseline",
		Source: domain.Source{
			GitURL: "https://github.com/example/research.git",
			Commit: strings.Repeat("a", 40),
		},
		Command: []string{"python", "train.py"},
		Retry: domain.RetryPolicy{
			MaxAttempts: 1,
			LostWorker:  domain.LostWorkerManual,
		},
	}
}

func TestCreateJobIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.db")

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateJob(ctx, "request-1", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created {
		t.Fatal("first request was not reported as created")
	}
	replayed, err := store.CreateJob(ctx, "request-1", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Created || replayed.Job.ID != created.Job.ID {
		t.Fatalf("idempotent replay = %+v, want existing %s", replayed, created.Job.ID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetJob(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobQueued || got.Spec.Name != created.Job.Spec.Name {
		t.Fatalf("reopened job = %+v", got)
	}
}

func TestCreateJobRejectsIdempotencyKeyReuse(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.CreateJob(ctx, "request-1", testSpec()); err != nil {
		t.Fatal(err)
	}
	changed := testSpec()
	changed.Name = "another-job"
	_, err = store.CreateJob(ctx, "request-1", changed)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
}
