package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func TestHealthPoliciesEnqueueDurableWebhookExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	startedAt := time.Date(2026, time.August, 23, 7, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return startedAt }
	const secret = "lcjh_0123456789abcdef0123456789abcdef0123456789abc"
	if _, err := store.CreateWebhookTarget(ctx, "research-session", "http://127.0.0.1/hook", secret); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Health = []domain.HealthPolicy{
		{Kind: domain.HealthKindPeriodic, Window: "10s", Action: domain.HealthActionNotify, Target: "research-session"},
		{Kind: domain.HealthKindMetricStalled, Metric: "validation/accuracy", Mode: domain.HealthModeMax, Window: "10s", MinimumDelta: 0.01, Action: domain.HealthActionNotify, Target: "research-session"},
	}
	created, err := store.CreateJob(ctx, "health-job", spec)
	if err != nil {
		t.Fatal(err)
	}
	missing := testSpec()
	missing.Health = []domain.HealthPolicy{{Kind: domain.HealthKindPeriodic, Window: "10s", Action: domain.HealthActionNotify, Target: "missing-target"}}
	if _, err := store.CreateJob(ctx, "missing-health-target", missing); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing target error = %v", err)
	}
	credential, err := store.CreateWorker(ctx, "health-worker", "lcjw_health0123456789abcdef0123456789abcdef012345678", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "health-session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offer, err := store.SyncWorker(ctx, credential.WorkerID, request)
	if err != nil || len(offer.Commands) != 1 {
		t.Fatalf("offer = %+v, %v", offer, err)
	}
	command := offer.Commands[0]
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "health-accepted", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: startedAt},
		{Sequence: 2, EventID: "health-started", AttemptID: command.AttemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: startedAt},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	now := startedAt.Add(11 * time.Second)
	store.now = func() time.Time { return now }
	stepOne, stepTwo := int64(1), int64(2)
	request.Events = []domain.WorkerEvent{
		{Sequence: 3, EventID: "health-metric-1", AttemptID: command.AttemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: startedAt.Add(2 * time.Second), Metric: &domain.MetricSample{Name: "validation/accuracy", Value: 0.9, Step: &stepOne, ObservedAt: startedAt.Add(2 * time.Second)}},
		{Sequence: 4, EventID: "health-metric-2", AttemptID: command.AttemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: startedAt.Add(10 * time.Second), Metric: &domain.MetricSample{Name: "validation/accuracy", Value: 0.9001, Step: &stepTwo, ObservedAt: startedAt.Add(10 * time.Second)}},
	}
	if _, err := store.SyncWorker(ctx, credential.WorkerID, request); err != nil {
		t.Fatal(err)
	}
	fired, err := store.EvaluateHealthPolicies(ctx)
	if err != nil || fired != 2 {
		t.Fatalf("fired = %d, %v", fired, err)
	}
	if repeated, err := store.EvaluateHealthPolicies(ctx); err != nil || repeated != 0 {
		t.Fatalf("repeated firing = %d, %v", repeated, err)
	}
	deliveries, err := store.DueWebhookDeliveries(ctx, 20)
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("deliveries = %+v, %v", deliveries, err)
	}
	for _, delivery := range deliveries {
		if delivery.TargetName != "research-session" || string(delivery.Secret) != secret || !strings.Contains(string(delivery.Payload), created.Job.ID) {
			t.Fatalf("delivery = %+v", delivery)
		}
	}
	if err := store.RecordWebhookResult(ctx, deliveries[0].ID, 503, "receiver unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordWebhookResult(ctx, deliveries[1].ID, 204, ""); err != nil {
		t.Fatal(err)
	}
	if due, err := store.DueWebhookDeliveries(ctx, 20); err != nil || len(due) != 0 {
		t.Fatalf("delivery retried before backoff = %+v, %v", due, err)
	}
	now = now.Add(5 * time.Second)
	retry, err := store.DueWebhookDeliveries(ctx, 20)
	if err != nil || len(retry) != 1 || retry[0].ID != deliveries[0].ID || retry[0].AttemptCount != 1 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	if err := store.RecordWebhookResult(ctx, retry[0].ID, 204, ""); err != nil {
		t.Fatal(err)
	}
	if due, err := store.DueWebhookDeliveries(ctx, 20); err != nil || len(due) != 0 {
		t.Fatalf("due after delivery = %+v, %v", due, err)
	}
	detail, err := store.GetJobDetail(ctx, created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Health) != 2 || detail.Health[0].Delivery.State != "delivered" || detail.Health[1].Delivery.State != "delivered" {
		t.Fatalf("health detail = %+v", detail.Health)
	}
	for table, want := range map[string]int{"health_firings": 2, "webhook_deliveries": 2} {
		var got int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
}
