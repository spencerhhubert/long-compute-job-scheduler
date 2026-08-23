package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

func TestJobMetricsJSONRequiresSessionAndPagesWithCursor(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, testToken)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.CreateJob(ctx, "metrics-json-job", testJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	const workerToken = "lcjw_metrics0123456789abcdef0123456789abcdef01234567"
	worker, err := store.CreateWorker(ctx, "worker-test", workerToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.WorkerSyncRequest{
		WorkerID: worker.WorkerID, SessionID: "metrics-session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offered, err := store.SyncWorker(ctx, worker.WorkerID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered.Commands) != 1 {
		t.Fatalf("commands = %+v, want one offer", offered.Commands)
	}
	attemptID := offered.Commands[0].AttemptID
	now := time.Now().UTC().Truncate(time.Second)
	step := int64(100)
	request.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "event-started", AttemptID: attemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now},
		{Sequence: 2, EventID: "event-metric-1", AttemptID: attemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now,
			Metric: &domain.MetricSample{Name: "accuracy", Value: 0.91, Step: &step, ObservedAt: now}},
		{Sequence: 3, EventID: "event-metric-2", AttemptID: attemptID, Kind: domain.WorkerEventMetricSample, OccurredAt: now,
			Metric: &domain.MetricSample{Name: domain.SystemMetricPrefix + "rss_bytes", Value: 1 << 30, ObservedAt: now}},
	}
	if _, err := store.SyncWorker(ctx, worker.WorkerID, request); err != nil {
		t.Fatal(err)
	}

	metricsPath := "/jobs/" + created.Job.ID + "/metrics.json"
	anonymous := httptest.NewRecorder()
	server.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous metrics status = %d, want 401", anonymous.Code)
	}

	const operatorKey = "lcjs_metrics0123456789abcdef0123456789abcdef01234567"
	if _, err := store.CreateAPIToken(ctx, "metrics browser", operatorKey); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"key": {operatorKey}}.Encode()
	loginRequest := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	server.ServeHTTP(loginResponse, loginRequest)
	cookies := loginResponse.Result().Cookies()
	if loginResponse.Code != http.StatusSeeOther || len(cookies) != 1 {
		t.Fatalf("login = %d with %d cookies", loginResponse.Code, len(cookies))
	}
	cookie := cookies[0]

	fetch := func(target string) (*httptest.ResponseRecorder, jobMetricsPayload) {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var payload jobMetricsPayload
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
		}
		return response, payload
	}

	response, payload := fetch(metricsPath)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", response.Code, response.Body.String())
	}
	if payload.JobState != domain.JobRunning || payload.Cursor <= 0 || len(payload.Metrics) != 2 {
		t.Fatalf("payload = %+v, want a running job, positive cursor, and two series", payload)
	}
	declared := payload.Metrics[0]
	if declared.Definition.Name != "accuracy" || declared.Definition.DisplayName != "Accuracy" ||
		len(declared.Definition.ReferenceLines) != 1 || len(declared.Points) != 1 {
		t.Fatalf("declared series = %+v", declared)
	}
	if point := declared.Points[0]; point.Attempt != 1 || point.Step == nil || *point.Step != step || point.Value != 0.91 {
		t.Fatalf("declared point = %+v", point)
	}
	system := payload.Metrics[1]
	if system.Definition.Name != domain.SystemMetricPrefix+"rss_bytes" ||
		system.Definition.Format != domain.MetricFormatBytes || len(system.Points) != 1 {
		t.Fatalf("system series = %+v", system)
	}

	incremental, incrementalPayload := fetch(metricsPath + "?after=" + strconv.FormatInt(payload.Cursor, 10))
	if incremental.Code != http.StatusOK {
		t.Fatalf("incremental status = %d", incremental.Code)
	}
	if incrementalPayload.Cursor != payload.Cursor || len(incrementalPayload.Metrics) != 1 || len(incrementalPayload.Metrics[0].Points) != 0 {
		t.Fatalf("incremental payload = %+v, want only the declared series with no new points", incrementalPayload)
	}

	if badAfter, _ := fetch(metricsPath + "?after=nonsense"); badAfter.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor status = %d, want 400", badAfter.Code)
	}
	if missing, _ := fetch("/jobs/job_missing/metrics.json"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing job status = %d, want 404", missing.Code)
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.Job.ID+"/metrics", nil)
	apiRequest.Header.Set("Authorization", "Bearer "+testToken)
	apiResponse := httptest.NewRecorder()
	server.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusOK {
		t.Fatalf("bearer metrics status = %d: %s", apiResponse.Code, apiResponse.Body.String())
	}
	apiAnonymous := httptest.NewRecorder()
	server.ServeHTTP(apiAnonymous, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.Job.ID+"/metrics", nil))
	if apiAnonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous bearer metrics status = %d, want 401", apiAnonymous.Code)
	}
}

func TestFormatMetricValueRendersBinaryByteUnits(t *testing.T) {
	definition := domain.MetricDefinition{Format: domain.MetricFormatBytes}
	for value, want := range map[float64]string{512: "512 B", 1536: "1.5 KiB", 2 << 30: "2.0 GiB"} {
		if got := formatMetricValue(definition, value); got != want {
			t.Errorf("formatMetricValue(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestStaticChartAssetsAreServed(t *testing.T) {
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, testToken)
	if err != nil {
		t.Fatal(err)
	}
	for path, marker := range map[string]string{
		"/static/uplot.js":  "leeoniya/uPlot",
		"/static/uplot.css": ".uplot",
		"/static/charts.js": "metrics.json",
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), marker) {
			t.Fatalf("%s status = %d, marker %q missing", path, response.Code, marker)
		}
	}
}
