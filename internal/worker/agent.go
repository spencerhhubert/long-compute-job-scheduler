package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
)

type Config struct {
	ServerURL    string
	WorkerID     string
	Token        string
	Version      string
	DatabasePath string
	WorkRoot     string
	ArtifactRoot string
	Capacity     domain.WorkerCapacity
	HTTPClient   *http.Client
}

type Agent struct {
	config    Config
	store     *Store
	sessionID string
	client    *http.Client
}

func New(ctx context.Context, config Config) (*Agent, error) {
	parsed, err := url.Parse(config.ServerURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		return nil, errors.New("worker server URL must use HTTPS outside loopback development")
	}
	config.ServerURL = strings.TrimRight(config.ServerURL, "/")
	if config.WorkerID == "" || len(config.Token) < 32 {
		return nil, errors.New("worker ID and token are required")
	}
	if config.WorkRoot == "" || config.ArtifactRoot == "" {
		return nil, errors.New("worker work and artifact roots are required")
	}
	if config.Capacity.MaxParallel == 0 {
		config.Capacity.MaxParallel = 1
	}
	store, err := OpenStore(ctx, config.DatabasePath)
	if err != nil {
		return nil, err
	}
	sessionID, err := id.New("ses")
	if err != nil {
		store.Close()
		return nil, err
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Agent{config: config, store: store, sessionID: sessionID, client: client}, nil
}

func (a *Agent) Close() error {
	return a.store.Close()
}

func (a *Agent) Run(ctx context.Context) error {
	delay := time.Duration(0)
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
		next, err := a.cycle(ctx)
		if err != nil {
			slog.Error("worker cycle failed", "error", err)
			delay = 5 * time.Second
			continue
		}
		delay = next
	}
}

func (a *Agent) cycle(ctx context.Context) (time.Duration, error) {
	if err := a.reconcileRunning(ctx); err != nil {
		return 0, err
	}
	events, err := a.store.Events(ctx, 1000)
	if err != nil {
		return 0, err
	}
	requestBody := domain.WorkerSyncRequest{
		WorkerID: a.config.WorkerID, SessionID: a.sessionID,
		AgentVersion: a.config.Version, Capacity: a.config.Capacity, Events: events,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.ServerURL+"/api/v1/worker/sync", bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+a.config.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("sync with control plane: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return 0, fmt.Errorf("control-plane sync returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var syncResponse domain.WorkerSyncResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&syncResponse); err != nil {
		return 0, fmt.Errorf("decode control-plane sync: %w", err)
	}
	// Commands are persisted before the events carried by this request are
	// acknowledged. Repeating either side of this boundary is harmless.
	if err := a.store.AcceptCommands(ctx, syncResponse.Commands); err != nil {
		return 0, err
	}
	if err := a.store.Acknowledge(ctx, syncResponse.AcceptedThrough); err != nil {
		return 0, err
	}
	if err := a.launchPending(ctx); err != nil {
		return 0, err
	}
	delay := time.Duration(syncResponse.NextSyncSeconds) * time.Second
	if delay < time.Second || delay > time.Minute {
		delay = 5 * time.Second
	}
	return delay, nil
}
