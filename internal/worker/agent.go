package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	ArtifactRoot string
	// Projects maps a project name to the absolute directory attempts for
	// that project run in. The worker advertises the names to the control
	// plane and is only offered jobs for projects it has a directory for.
	Projects   map[string]string
	Capacity   domain.WorkerCapacity
	HTTPClient *http.Client
}

type Agent struct {
	config    Config
	store     *Store
	sessionID string
	client    *http.Client
	// samplers tracks the per-attempt resource sampler goroutines. It is
	// only touched from the agent's single cycle goroutine.
	samplers map[string]context.CancelFunc
	// lastArtifactSweep and lastArtifactHash bound how often a running
	// attempt re-publishes its output, and to what actually changed. Same
	// goroutine, same lack of locking.
	lastArtifactSweep map[string]time.Time
	lastArtifactHash  map[string]map[string]string
}

// artifactInterval is how often a running attempt's declared artifacts are
// re-collected and announced. Long enough that a checkpoint being rewritten
// every few seconds does not flood the event stream, short enough that a
// fifteen-minute poll always sees something recent.
const artifactInterval = 60 * time.Second

func New(ctx context.Context, config Config) (*Agent, error) {
	parsed, err := url.Parse(config.ServerURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		return nil, errors.New("worker server URL must use HTTPS outside loopback development")
	}
	config.ServerURL = strings.TrimRight(config.ServerURL, "/")
	if config.WorkerID == "" || len(config.Token) < 32 {
		return nil, errors.New("worker ID and token are required")
	}
	if config.ArtifactRoot == "" {
		return nil, errors.New("worker artifact root is required")
	}
	if len(config.Projects) == 0 {
		return nil, errors.New("at least one --project name=/absolute/path mapping is required")
	}
	for name, directory := range config.Projects {
		if name == "" || !filepath.IsAbs(directory) {
			return nil, fmt.Errorf("project %q must map a name to an absolute directory", name)
		}
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("project %q directory %s is not an existing directory", name, directory)
		}
	}
	config.Capacity.Projects = slices.Sorted(maps.Keys(config.Projects))
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
	return &Agent{config: config, store: store, sessionID: sessionID, client: client, samplers: make(map[string]context.CancelFunc)}, nil
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
