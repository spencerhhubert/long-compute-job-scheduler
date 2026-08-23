package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func testSpec() domain.JobSpec {
	precision := uint8(2)
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
		Metrics: []domain.MetricDefinition{{
			Name: "validation/accuracy", Format: domain.MetricFormatPercent,
			Precision: &precision, Objective: domain.MetricObjectiveMaximize,
			ReferenceLines: []domain.MetricReferenceLine{{
				Label: "Goal", Value: 0.95, Kind: domain.MetricReferenceGoal,
			}},
		}},
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
	if got.State != domain.JobQueued || got.Spec.Name != created.Job.Spec.Name || len(got.Spec.Metrics) != 1 || got.Spec.Metrics[0].ReferenceLines[0].Value != 0.95 {
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

func TestCancelQueuedJobIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, err := store.CreateJob(ctx, "cancel-job", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := store.CancelJob(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != domain.JobCanceled || canceled.Revision != created.Job.Revision+1 {
		t.Fatalf("canceled job = %+v", canceled)
	}
	repeated, err := store.CancelJob(ctx, created.Job.ID)
	if err != nil || repeated.State != domain.JobCanceled || repeated.Revision != canceled.Revision {
		t.Fatalf("repeated cancellation = %+v, %v", repeated, err)
	}
}

func TestOperatorTokenAndBrowserSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	const rawToken = "lcjs_0123456789abcdef0123456789abcdef0123456789abc"
	token, err := store.CreateAPIToken(ctx, "primary browser", rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if token.Scope != OperatorScope || token.Name != "primary browser" {
		t.Fatalf("created token = %+v", token)
	}
	authenticated, err := store.AuthenticateAPIToken(ctx, rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != token.ID {
		t.Fatalf("authenticated token ID = %q, want %q", authenticated.ID, token.ID)
	}
	if _, err := store.AuthenticateAPIToken(ctx, "wrong-token-value-that-is-long-enough"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid token error = %v, want ErrNotFound", err)
	}

	var stored []byte
	if err := store.db.QueryRowContext(ctx, "SELECT token_hash FROM api_tokens WHERE id = ?", token.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(rawToken))
	if string(stored) != string(wantHash[:]) || string(stored) == rawToken {
		t.Fatal("database did not contain exactly the token hash")
	}

	const rawSession = "lcss_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	if err := store.CreateBrowserSession(ctx, token.ID, rawSession, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	session, err := store.AuthenticateBrowserSession(ctx, rawSession)
	if err != nil {
		t.Fatal(err)
	}
	if session.TokenID != token.ID || session.TokenName != token.Name {
		t.Fatalf("authenticated session = %+v", session)
	}
	now = now.Add(2 * time.Hour)
	if _, err := store.AuthenticateBrowserSession(ctx, rawSession); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session error = %v, want ErrNotFound", err)
	}

	now = now.Add(-time.Hour)
	const secondSession = "lcss_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg"
	if err := store.CreateBrowserSession(ctx, token.ID, secondSession, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBrowserSession(ctx, secondSession); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateBrowserSession(ctx, secondSession); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session error = %v, want ErrNotFound", err)
	}
}
