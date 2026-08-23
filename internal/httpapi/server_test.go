package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestConsoleLoginPersistsSecureSessionAndShowsJobState(t *testing.T) {
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, testToken)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated response = %d, location = %q", response.Code, response.Header().Get("Location"))
	}

	loginPageRequest := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginPageResponse := httptest.NewRecorder()
	server.ServeHTTP(loginPageResponse, loginPageRequest)
	if loginPageResponse.Code != http.StatusOK {
		t.Fatalf("login page status = %d", loginPageResponse.Code)
	}
	if body := loginPageResponse.Body.String(); !strings.Contains(body, "Operator sign in") || !strings.Contains(body, `data-theme="light"`) {
		t.Fatalf("login page = %q", body)
	}
	if !strings.Contains(loginPageResponse.Body.String(), `/static/app.css?v=4`) {
		t.Fatal("login page does not use the current stylesheet version")
	}

	const operatorKey = "lcjs_0123456789abcdef0123456789abcdef0123456789abc"
	if _, err := store.CreateAPIToken(context.Background(), "test browser", operatorKey); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateJob(context.Background(), "dashboard-job", testJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	const workerToken = "lcjw_dashboard0123456789abcdef0123456789abcdef0123456"
	worker, err := store.CreateWorker(context.Background(), "worker-test", workerToken, map[string]string{"host": "gpu-worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncWorker(context.Background(), worker.WorkerID, domain.WorkerSyncRequest{
		WorkerID: worker.WorkerID, SessionID: "dashboard-session", AgentVersion: "test-sha",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Labels: map[string]string{"host": "gpu-worker-1"}, Projects: []string{"example-research"}},
	}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"key": {operatorKey}}.Encode()
	loginRequest := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	server.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusSeeOther || loginResponse.Header().Get("Location") != "/" {
		t.Fatalf("login response = %d, location = %q", loginResponse.Code, loginResponse.Header().Get("Location"))
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookie || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge <= 0 {
		t.Fatalf("session cookie = %+v", cookie)
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	dashboardRequest.AddCookie(cookie)
	dashboardResponse := httptest.NewRecorder()
	server.ServeHTTP(dashboardResponse, dashboardRequest)
	if dashboardResponse.Code != http.StatusOK {
		t.Fatalf("status = %d", dashboardResponse.Code)
	}
	if contentType := dashboardResponse.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if policy := dashboardResponse.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "default-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q", policy)
	}
	body := dashboardResponse.Body.String()
	for _, expected := range []string{"Control plane online", "train-baseline", "example-research", "Human best", "94.5%", "worker-test", "4 CPU", "host=gpu-worker-1", "test-sha", "test browser", "/jobs/" + created.Job.ID} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard does not contain %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "Long Compute Job Scheduler") {
		t.Fatal("dashboard still contains the old landing-page title")
	}
	detailRequest := httptest.NewRequest(http.MethodGet, "/jobs/"+created.Job.ID, nil)
	detailRequest.AddCookie(cookie)
	detailResponse := httptest.NewRecorder()
	server.ServeHTTP(detailResponse, detailRequest)
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("job page status = %d: %s", detailResponse.Code, detailResponse.Body.String())
	}
	for _, expected := range []string{
		"Attempts", "Metrics", "Artifacts", worker.WorkerID,
		`data-metric-charts data-job-id="` + created.Job.ID + `"`,
		"/static/uplot.js", "/static/uplot.css", "/static/charts.js",
	} {
		if !strings.Contains(detailResponse.Body.String(), expected) {
			t.Fatalf("job page does not contain %q: %s", expected, detailResponse.Body.String())
		}
	}
	if policy := detailResponse.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "connect-src 'self'") {
		t.Fatalf("job page Content-Security-Policy = %q, want connect-src for chart polling", policy)
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutRequest.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	server.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d", logoutResponse.Code)
	}
	afterLogout := httptest.NewRequest(http.MethodGet, "/", nil)
	afterLogout.AddCookie(cookie)
	afterLogoutResponse := httptest.NewRecorder()
	server.ServeHTTP(afterLogoutResponse, afterLogout)
	if afterLogoutResponse.Code != http.StatusSeeOther {
		t.Fatalf("status after logout = %d", afterLogoutResponse.Code)
	}
}

func TestJobPagePreviewsSmallTextArtifacts(t *testing.T) {
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
	const operatorKey = "lcjs_preview0123456789abcdef0123456789abcdef0123"
	if _, err := store.CreateAPIToken(ctx, "preview browser", operatorKey); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateJob(ctx, "preview-job", testJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateWorker(ctx, "worker-preview", "lcjw_preview0123456789abcdef0123456789abcdef0123", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncRequest := domain.WorkerSyncRequest{
		WorkerID: worker.WorkerID, SessionID: "preview-session", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	}
	offered, err := store.SyncWorker(ctx, worker.WorkerID, syncRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered.Commands) != 1 {
		t.Fatalf("offered commands = %+v", offered.Commands)
	}
	attemptID := offered.Commands[0].AttemptID
	exitCode := 0
	now := time.Now().UTC()
	const jsonContent = `{"note":"<script>alert(1)</script>","score":0.91}`
	const textContent = "plain <b>text</b> & more"
	syncRequest.Events = []domain.WorkerEvent{
		{Sequence: 1, EventID: "pv-accepted", AttemptID: attemptID, Kind: domain.WorkerEventAttemptAccepted, OccurredAt: now},
		{Sequence: 2, EventID: "pv-started", AttemptID: attemptID, Kind: domain.WorkerEventAttemptStarted, OccurredAt: now},
		{Sequence: 3, EventID: "pv-finished", AttemptID: attemptID, Kind: domain.WorkerEventAttemptFinished, OccurredAt: now, ExitCode: &exitCode, Artifacts: []domain.ArtifactAnnouncement{
			{Name: "result", URI: "worker://worker-preview/p/j/1/result/champion.json", SizeBytes: int64(len(jsonContent)), SHA256: "aaa", Content: jsonContent},
			{Name: "notes", URI: "worker://worker-preview/p/j/1/notes/notes.txt", SizeBytes: int64(len(textContent)), SHA256: "bbb", Content: textContent},
			{Name: "checkpoint", URI: "worker://worker-preview/p/j/1/checkpoint/model.pt", SizeBytes: 1 << 30, SHA256: "ccc"},
		}},
	}
	if _, err := store.SyncWorker(ctx, worker.WorkerID, syncRequest); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"key": {operatorKey}}.Encode()
	loginRequest := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	server.ServeHTTP(loginResponse, loginRequest)
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %d, want 1", len(cookies))
	}
	pageRequest := httptest.NewRequest(http.MethodGet, "/jobs/"+created.Job.ID, nil)
	pageRequest.AddCookie(cookies[0])
	pageResponse := httptest.NewRecorder()
	server.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("job page status = %d: %s", pageResponse.Code, pageResponse.Body.String())
	}
	body := pageResponse.Body.String()
	for _, expected := range []string{
		// The JSON artifact is pretty-printed (the raw content has no space
		// after the colon) and escaped.
		"&#34;score&#34;: 0.91",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		// The plain-text artifact is shown as-is, escaped.
		"plain &lt;b&gt;text&lt;/b&gt; &amp; more",
		// The metadata-only artifact still appears in the table.
		"model.pt",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("job page does not contain %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("job page contains unescaped artifact content")
	}
	if got := strings.Count(body, `<details class="log-tail"><summary>`); got != 2 {
		t.Fatalf("artifact previews = %d, want 2 (none for the metadata-only artifact)", got)
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.Job.ID, nil)
	apiRequest.Header.Set("Authorization", "Bearer "+testToken)
	apiResponse := httptest.NewRecorder()
	server.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusOK {
		t.Fatalf("API job detail status = %d: %s", apiResponse.Code, apiResponse.Body.String())
	}
	var detail domain.JobDetail
	if err := json.Unmarshal(apiResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	contents := make(map[string]string, len(detail.Artifacts))
	for _, artifact := range detail.Artifacts {
		contents[artifact.Name] = artifact.Content
	}
	if contents["result"] != jsonContent || contents["notes"] != textContent || contents["checkpoint"] != "" {
		t.Fatalf("API artifact contents = %+v", contents)
	}
}

func TestLoginRejectsInvalidKey(t *testing.T) {
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, testToken)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"key": {"not-a-valid-key"}}.Encode()
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "That key is not valid") {
		t.Fatalf("invalid login response = %d: %s", response.Code, response.Body.String())
	}
}

func TestJobsRequireAuthenticationAndCreateIdempotently(t *testing.T) {
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, testToken)
	if err != nil {
		t.Fatal(err)
	}

	unauthenticated := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	unauthenticatedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticatedResponse.Code)
	}

	spec := domain.JobSpec{
		Project: "example-research",
		Name:    "train-baseline",
		Source: domain.Source{
			GitURL: "https://github.com/example/research.git",
			Commit: strings.Repeat("a", 40),
		},
		Command: []string{"python", "train.py"},
		Retry:   domain.RetryPolicy{MaxAttempts: 1, LostWorker: domain.LostWorkerManual},
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	var createdID string
	for attempt, wantStatus := range []int{http.StatusCreated, http.StatusOK} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Idempotency-Key", "request-1")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != wantStatus {
			t.Fatalf("attempt %d status = %d, body = %s", attempt, response.Code, response.Body.String())
		}
		var job domain.Job
		if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			createdID = job.ID
		} else if job.ID != createdID {
			t.Fatalf("replayed ID = %s, want %s", job.ID, createdID)
		}
	}
	detailRequest := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+createdID, nil)
	detailRequest.Header.Set("Authorization", "Bearer "+testToken)
	detailResponse := httptest.NewRecorder()
	server.ServeHTTP(detailResponse, detailRequest)
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("job detail status = %d: %s", detailResponse.Code, detailResponse.Body.String())
	}
	var detail domain.JobDetail
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Job.ID != createdID || detail.Attempts == nil || detail.Metrics == nil || detail.Artifacts == nil || detail.Health == nil {
		t.Fatalf("job detail = %+v", detail)
	}
	cancelRequest := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+createdID+"/cancel", nil)
	cancelRequest.Header.Set("Authorization", "Bearer "+testToken)
	cancelResponse := httptest.NewRecorder()
	server.ServeHTTP(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusOK {
		t.Fatalf("cancel status = %d: %s", cancelResponse.Code, cancelResponse.Body.String())
	}
	var canceled domain.Job
	if err := json.Unmarshal(cancelResponse.Body.Bytes(), &canceled); err != nil {
		t.Fatal(err)
	}
	if canceled.State != domain.JobCanceled {
		t.Fatalf("canceled job = %+v", canceled)
	}

	const databaseToken = "lcjs_fedcba9876543210fedcba9876543210fedcba987654"
	if _, err := store.CreateAPIToken(context.Background(), "api test", databaseToken); err != nil {
		t.Fatal(err)
	}
	databaseTokenRequest := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	databaseTokenRequest.Header.Set("Authorization", "Bearer "+databaseToken)
	databaseTokenResponse := httptest.NewRecorder()
	server.ServeHTTP(databaseTokenResponse, databaseTokenRequest)
	if databaseTokenResponse.Code != http.StatusOK {
		t.Fatalf("database token status = %d: %s", databaseTokenResponse.Code, databaseTokenResponse.Body.String())
	}
}

func TestWorkerSyncUsesWorkerBoundToken(t *testing.T) {
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, testToken)
	if err != nil {
		t.Fatal(err)
	}
	const workerToken = "lcjw_0123456789abcdef0123456789abcdef0123456789abc"
	credential, err := store.CreateWorker(context.Background(), "worker-test", workerToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJob(context.Background(), "worker-http-job", testJobSpec()); err != nil {
		t.Fatal(err)
	}
	syncBody, err := json.Marshal(domain.WorkerSyncRequest{
		WorkerID: credential.WorkerID, SessionID: "session-test", AgentVersion: "test",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Projects: []string{"example-research"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/sync", bytes.NewReader(syncBody))
	request.Header.Set("Authorization", "Bearer "+workerToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("worker sync status = %d: %s", response.Code, response.Body.String())
	}
	var syncResponse domain.WorkerSyncResponse
	if err := json.Unmarshal(response.Body.Bytes(), &syncResponse); err != nil {
		t.Fatal(err)
	}
	if len(syncResponse.Commands) != 1 {
		t.Fatalf("worker sync commands = %+v", syncResponse.Commands)
	}

	mismatch := domain.WorkerSyncRequest{WorkerID: "wrk_someone_else", SessionID: "session"}
	mismatchBody, _ := json.Marshal(mismatch)
	mismatchRequest := httptest.NewRequest(http.MethodPost, "/api/v1/worker/sync", bytes.NewReader(mismatchBody))
	mismatchRequest.Header.Set("Authorization", "Bearer "+workerToken)
	mismatchResponse := httptest.NewRecorder()
	server.ServeHTTP(mismatchResponse, mismatchRequest)
	if mismatchResponse.Code != http.StatusForbidden {
		t.Fatalf("identity mismatch status = %d", mismatchResponse.Code)
	}
}

func testJobSpec() domain.JobSpec {
	precision := uint8(1)
	return domain.JobSpec{
		Project: "example-research",
		Name:    "train-baseline",
		Source: domain.Source{
			GitURL: "https://github.com/example/research.git",
			Commit: strings.Repeat("a", 40),
		},
		Command: []string{"python", "train.py"},
		Retry:   domain.RetryPolicy{MaxAttempts: 1, LostWorker: domain.LostWorkerManual},
		Metrics: []domain.MetricDefinition{{
			Name: "accuracy", DisplayName: "Accuracy", Format: domain.MetricFormatPercent,
			Precision: &precision, Objective: domain.MetricObjectiveMaximize,
			ReferenceLines: []domain.MetricReferenceLine{{
				Label: "Human best", Value: 0.945, Kind: domain.MetricReferenceBenchmark,
			}},
		}},
	}
}
