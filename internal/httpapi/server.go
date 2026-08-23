// Package httpapi exposes the control-plane domain through a versioned JSON
// API and a compact operator console.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

const (
	maxRequestBody = 1 << 20
	sessionCookie  = "lcjs_session"
	sessionLife    = 90 * 24 * time.Hour
)

type Store interface {
	CreateJob(context.Context, string, domain.JobSpec) (sqlitestore.CreateJobResult, error)
	GetJob(context.Context, string) (domain.Job, error)
	ListJobs(context.Context, int) ([]domain.Job, error)
	AuthenticateAPIToken(context.Context, string) (sqlitestore.APIToken, error)
	CreateBrowserSession(context.Context, string, string, time.Time) error
	AuthenticateBrowserSession(context.Context, string) (sqlitestore.BrowserSession, error)
	DeleteBrowserSession(context.Context, string) error
	AuthenticateWorkerToken(context.Context, string) (sqlitestore.WorkerCredential, error)
	SyncWorker(context.Context, string, domain.WorkerSyncRequest) (domain.WorkerSyncResponse, error)
	ListWorkers(context.Context, time.Duration) ([]domain.Worker, error)
}

type Server struct {
	store     Store
	tokenHash [32]byte
	handler   http.Handler
}

func New(store Store, bootstrapToken string) (*Server, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if len(bootstrapToken) < 32 {
		return nil, errors.New("bootstrap token must be at least 32 characters")
	}
	s := &Server{store: store, tokenHash: sha256.Sum256([]byte(bootstrapToken))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /static/app.css", serveStyles)
	mux.HandleFunc("GET /static/theme.js", serveThemeScript)
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("POST /api/v1/jobs", s.authenticateAPI(http.HandlerFunc(s.createJob)))
	mux.Handle("GET /api/v1/jobs", s.authenticateAPI(http.HandlerFunc(s.listJobs)))
	mux.Handle("GET /api/v1/jobs/{id}", s.authenticateAPI(http.HandlerFunc(s.getJob)))
	mux.Handle("GET /api/v1/workers", s.authenticateAPI(http.HandlerFunc(s.listWorkers)))
	mux.HandleFunc("POST /api/v1/worker/sync", s.workerSync)
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

func (s *Server) authenticateAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		const prefix = "Bearer "
		header := request.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			writeError(response, http.StatusUnauthorized, "unauthorized", "a bearer token is required")
			return
		}
		rawToken := strings.TrimPrefix(header, prefix)
		provided := sha256.Sum256([]byte(rawToken))
		if subtle.ConstantTimeCompare(provided[:], s.tokenHash[:]) != 1 {
			_, err := s.store.AuthenticateAPIToken(request.Context(), rawToken)
			if errors.Is(err, sqlitestore.ErrNotFound) {
				writeError(response, http.StatusUnauthorized, "unauthorized", "the bearer token is invalid")
				return
			}
			if err != nil {
				writeError(response, http.StatusInternalServerError, "internal", "authentication is temporarily unavailable")
				return
			}
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) home(response http.ResponseWriter, request *http.Request) {
	session, _, err := s.browserSession(request)
	if errors.Is(err, sqlitestore.ErrNotFound) {
		http.Redirect(response, request, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(response, "the console is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	jobs, err := s.store.ListJobs(request.Context(), 1000)
	if err != nil {
		http.Error(response, "the console is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	data := dashboardView{TokenName: session.TokenName, Jobs: jobs}
	for _, job := range jobs {
		switch job.State {
		case domain.JobQueued, domain.JobRetryWait:
			data.Queued++
		case domain.JobRunning, domain.JobCanceling:
			data.Active++
		case domain.JobFailed:
			data.Failed++
		case domain.JobSucceeded:
			data.Succeeded++
		}
	}
	renderPage(response, dashboardTemplate, data)
}

func (s *Server) loginPage(response http.ResponseWriter, request *http.Request) {
	if _, _, err := s.browserSession(request); err == nil {
		http.Redirect(response, request, "/", http.StatusSeeOther)
		return
	} else if !errors.Is(err, sqlitestore.ErrNotFound) {
		http.Error(response, "login is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	renderPage(response, loginTemplate, loginView{})
}

func (s *Server) login(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		renderPageStatus(response, loginTemplate, loginView{Error: "That key could not be used."}, http.StatusBadRequest)
		return
	}
	rawToken := strings.TrimSpace(request.PostForm.Get("key"))
	token, err := s.store.AuthenticateAPIToken(request.Context(), rawToken)
	if errors.Is(err, sqlitestore.ErrNotFound) {
		renderPageStatus(response, loginTemplate, loginView{Error: "That key is not valid."}, http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(response, "login is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	rawSession, err := randomSecret("lcss_")
	if err != nil {
		http.Error(response, "login is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().UTC().Add(sessionLife)
	if err := s.store.CreateBrowserSession(request.Context(), token.ID, rawSession, expiresAt); err != nil {
		http.Error(response, "login is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(response, &http.Cookie{
		Name: sessionCookie, Value: rawSession, Path: "/", Expires: expiresAt,
		MaxAge: int(sessionLife.Seconds()), Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(response, request, "/", http.StatusSeeOther)
}

func (s *Server) logout(response http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteBrowserSession(request.Context(), cookie.Value)
	}
	http.SetCookie(response, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0), Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(response, request, "/login", http.StatusSeeOther)
}

func (s *Server) browserSession(request *http.Request) (sqlitestore.BrowserSession, string, error) {
	cookie, err := request.Cookie(sessionCookie)
	if errors.Is(err, http.ErrNoCookie) || cookie.Value == "" {
		return sqlitestore.BrowserSession{}, "", sqlitestore.ErrNotFound
	}
	if err != nil {
		return sqlitestore.BrowserSession{}, "", err
	}
	session, err := s.store.AuthenticateBrowserSession(request.Context(), cookie.Value)
	return session, cookie.Value, err
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

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

func (s *Server) listWorkers(response http.ResponseWriter, request *http.Request) {
	workers, err := s.store.ListWorkers(request.Context(), 30*time.Second)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "workers could not be listed")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"workers": workers})
}

func (s *Server) workerSync(response http.ResponseWriter, request *http.Request) {
	const prefix = "Bearer "
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		writeError(response, http.StatusUnauthorized, "unauthorized", "a worker bearer token is required")
		return
	}
	credential, err := s.store.AuthenticateWorkerToken(request.Context(), strings.TrimPrefix(header, prefix))
	if errors.Is(err, sqlitestore.ErrNotFound) {
		writeError(response, http.StatusUnauthorized, "unauthorized", "the worker bearer token is invalid")
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "worker authentication is temporarily unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var syncRequest domain.WorkerSyncRequest
	if err := decoder.Decode(&syncRequest); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", "request body must be one worker sync request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(response, http.StatusBadRequest, "invalid_json", "request body must contain exactly one JSON value")
		return
	}
	if syncRequest.WorkerID != credential.WorkerID {
		writeError(response, http.StatusForbidden, "worker_identity_mismatch", "the token is not bound to this worker")
		return
	}
	if syncRequest.SessionID == "" || len(syncRequest.Events) > 1000 {
		writeError(response, http.StatusBadRequest, "invalid_sync", "session_id is required and events are limited to 1000 per sync")
		return
	}
	syncResponse, err := s.store.SyncWorker(request.Context(), credential.WorkerID, syncRequest)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "worker sync could not be applied")
		return
	}
	writeJSON(response, http.StatusOK, syncResponse)
}

func randomSecret(prefix string) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
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
