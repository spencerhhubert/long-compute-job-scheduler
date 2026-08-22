package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

const testToken = "0123456789abcdef0123456789abcdef"

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
}
