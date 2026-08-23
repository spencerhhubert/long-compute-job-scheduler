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
	if !strings.Contains(loginPageResponse.Body.String(), `/static/app.css?v=3`) {
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
	worker, err := store.CreateWorker(context.Background(), "worker-test", workerToken, map[string]string{"host": "entropy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncWorker(context.Background(), worker.WorkerID, domain.WorkerSyncRequest{
		WorkerID: worker.WorkerID, SessionID: "dashboard-session", AgentVersion: "test-sha",
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1, Labels: map[string]string{"host": "entropy"}},
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
	for _, expected := range []string{"Control plane online", "train-baseline", "example-research", "Human best", "94.5%", "worker-test", "4 CPU", "host=entropy", "test-sha", "test browser", "/jobs/" + created.Job.ID} {
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
	for _, expected := range []string{"Attempts", "Metrics", "Artifacts", "Human best", "No samples", worker.WorkerID} {
		if !strings.Contains(detailResponse.Body.String(), expected) {
			t.Fatalf("job page does not contain %q: %s", expected, detailResponse.Body.String())
		}
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
		Capacity: domain.WorkerCapacity{CPU: 4, MaxParallel: 1},
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
