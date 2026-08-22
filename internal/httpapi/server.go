// Package httpapi exposes the control-plane domain through a versioned JSON
// API.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

const maxRequestBody = 1 << 20

type JobStore interface {
	CreateJob(context.Context, string, domain.JobSpec) (sqlitestore.CreateJobResult, error)
	GetJob(context.Context, string) (domain.Job, error)
	ListJobs(context.Context, int) ([]domain.Job, error)
}

type Server struct {
	store     JobStore
	tokenHash [32]byte
	handler   http.Handler
}

func New(store JobStore, bootstrapToken string) (*Server, error) {
	if store == nil {
		return nil, errors.New("job store is required")
	}
	if len(bootstrapToken) < 32 {
		return nil, errors.New("bootstrap token must be at least 32 characters")
	}
	s := &Server{store: store, tokenHash: sha256.Sum256([]byte(bootstrapToken))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("POST /api/v1/jobs", s.authenticate(http.HandlerFunc(s.createJob)))
	mux.Handle("GET /api/v1/jobs", s.authenticate(http.HandlerFunc(s.listJobs)))
	mux.Handle("GET /api/v1/jobs/{id}", s.authenticate(http.HandlerFunc(s.getJob)))
	s.handler = mux
	return s, nil
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID, err := id.New("req")
	if err == nil {
		response.Header().Set("X-Request-ID", requestID)
	}
	s.handler.ServeHTTP(response, request)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		const prefix = "Bearer "
		header := request.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			writeError(response, http.StatusUnauthorized, "unauthorized", "a bearer token is required")
			return
		}
		provided := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
		if subtle.ConstantTimeCompare(provided[:], s.tokenHash[:]) != 1 {
			writeError(response, http.StatusUnauthorized, "unauthorized", "the bearer token is invalid")
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) home(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(response, homePage)
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

const homePage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark">
  <title>Long Compute Job Scheduler</title>
  <style>
    :root { color-scheme: dark; font-family: ui-sans-serif, system-ui, sans-serif; }
    * { box-sizing: border-box; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #090b10; color: #eef1f6; }
    main { width: min(42rem, calc(100% - 2rem)); padding: 2.5rem; border: 1px solid #262b36; border-radius: 1.25rem; background: #11141b; box-shadow: 0 1.5rem 4rem #0008; }
    .status { display: inline-flex; align-items: center; gap: .55rem; color: #9ee6b4; font-size: .8rem; font-weight: 650; letter-spacing: .08em; text-transform: uppercase; }
    .status::before { content: ""; width: .6rem; height: .6rem; border-radius: 50%; background: #4ade80; box-shadow: 0 0 1rem #4ade8088; }
    h1 { margin: 1rem 0 .75rem; font-size: clamp(2rem, 7vw, 3.6rem); line-height: 1; letter-spacing: -.055em; }
    p { margin: 0; max-width: 34rem; color: #a8afbd; font-size: 1.05rem; line-height: 1.65; }
    footer { margin-top: 2.25rem; padding-top: 1.25rem; border-top: 1px solid #262b36; color: #71798a; font-size: .85rem; }
    a { color: #cbd5e1; text-underline-offset: .2em; }
  </style>
</head>
<body>
  <main>
    <div class="status">Control plane online</div>
    <h1>Long Compute<br>Job Scheduler</h1>
    <p>A durable control plane for heterogeneous, long-running compute jobs. The authenticated job dashboard and worker fleet are the next implementation slice.</p>
    <footer>Pre-alpha · <a href="https://github.com/spencerhhubert/long-compute-job-scheduler">Source on GitHub</a></footer>
  </main>
</body>
</html>
`

func (s *Server) createJob(response http.ResponseWriter, request *http.Request) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		writeError(response, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must be at most 200 characters")
		return
	}

	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var spec domain.JobSpec
	if err := decoder.Decode(&spec); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", "request body must be one JSON job specification")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(response, http.StatusBadRequest, "invalid_json", "request body must contain exactly one JSON value")
		return
	}

	result, err := s.store.CreateJob(request.Context(), key, spec)
	if err != nil {
		switch {
		case errors.Is(err, sqlitestore.ErrIdempotencyConflict):
			writeError(response, http.StatusConflict, "idempotency_conflict", err.Error())
		case strings.HasPrefix(err.Error(), "validate job:"):
			writeError(response, http.StatusUnprocessableEntity, "invalid_job", strings.TrimPrefix(err.Error(), "validate job: "))
		default:
			writeError(response, http.StatusInternalServerError, "internal", "the job could not be stored")
		}
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
		response.Header().Set("Location", "/api/v1/jobs/"+result.Job.ID)
	}
	writeJSON(response, status, result.Job)
}

func (s *Server) getJob(response http.ResponseWriter, request *http.Request) {
	job, err := s.store.GetJob(request.Context(), request.PathValue("id"))
	if errors.Is(err, sqlitestore.ErrNotFound) {
		writeError(response, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "the job could not be read")
		return
	}
	writeJSON(response, http.StatusOK, job)
}

func (s *Server) listJobs(response http.ResponseWriter, request *http.Request) {
	limit := 100
	if raw := request.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(response, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 1000")
			return
		}
		limit = parsed
	}
	jobs, err := s.store.ListJobs(request.Context(), limit)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "jobs could not be listed")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"jobs": jobs})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, map[string]any{
		"error": map[string]string{
			"code":       code,
			"message":    message,
			"request_id": response.Header().Get("X-Request-ID"),
		},
	})
}
