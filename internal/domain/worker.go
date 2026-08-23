package domain

import "time"

type AttemptState string

const (
	AttemptOffered   AttemptState = "offered"
	AttemptAccepted  AttemptState = "accepted"
	AttemptRunning   AttemptState = "running"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptCanceled  AttemptState = "canceled"
)

type WorkerStatus string

const (
	WorkerOnline  WorkerStatus = "online"
	WorkerOffline WorkerStatus = "offline"
)

type WorkerCapacity struct {
	CPU            uint32            `json:"cpu"`
	MemoryBytes    uint64            `json:"memory_bytes"`
	GPUCount       uint32            `json:"gpu_count"`
	GPUSharedSlots uint32            `json:"gpu_shared_slots"`
	MaxParallel    uint32            `json:"max_parallel"`
	Labels         map[string]string `json:"labels,omitempty"`
	// Projects lists the project names this worker has a local project
	// directory for. A job is offered only to workers that advertise its
	// project.
	Projects []string `json:"projects,omitempty"`
}

type Worker struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Status       WorkerStatus   `json:"status"`
	AgentVersion string         `json:"agent_version,omitempty"`
	Capacity     WorkerCapacity `json:"capacity"`
	LastSeenAt   *time.Time     `json:"last_seen_at,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

type WorkerEventKind string

const (
	WorkerEventAttemptAccepted WorkerEventKind = "attempt_accepted"
	WorkerEventAttemptStarted  WorkerEventKind = "attempt_started"
	WorkerEventAttemptFinished WorkerEventKind = "attempt_finished"
	WorkerEventMetricSample    WorkerEventKind = "metric_sample"
	// WorkerEventArtifacts records output a run has produced so far,
	// without ending the attempt. A long run's artifacts are the main
	// thing worth looking at while it is still going, and waiting until
	// it finishes to announce them makes them useless for watching it.
	WorkerEventArtifacts WorkerEventKind = "artifacts"
)

type MetricSample struct {
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Step       *int64    `json:"step,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type ArtifactAnnouncement struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	// Content is the artifact's full bytes, included only when the file is
	// at most 64 KiB of valid UTF-8 text. Larger or binary artifacts
	// announce metadata only.
	Content string `json:"content,omitempty"`
}

// GitState is the version-control state a worker observed in its project
// directory when an attempt started. It is recorded for reproducibility, not
// enforced. A nil GitState means the project directory is not a git
// repository.
type GitState struct {
	Commit string `json:"commit"`
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty"`
}

type Attempt struct {
	ID             string       `json:"id"`
	JobID          string       `json:"job_id"`
	AttemptNumber  uint32       `json:"attempt_number"`
	WorkerID       string       `json:"worker_id"`
	State          AttemptState `json:"state"`
	Revision       uint64       `json:"revision"`
	OfferedAt      time.Time    `json:"offered_at"`
	LeaseExpiresAt time.Time    `json:"lease_expires_at"`
	AcceptedAt     *time.Time   `json:"accepted_at,omitempty"`
	StartedAt      *time.Time   `json:"started_at,omitempty"`
	FinishedAt     *time.Time   `json:"finished_at,omitempty"`
	ExitCode       *int         `json:"exit_code,omitempty"`
	Error          string       `json:"error,omitempty"`
	LogURI         string       `json:"log_uri,omitempty"`
	LogTail        string       `json:"log_tail,omitempty"`
	Git            *GitState    `json:"git,omitempty"`
}

type RecordedMetric struct {
	AttemptID  string    `json:"attempt_id"`
	EventID    string    `json:"event_id"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Step       *int64    `json:"step,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type Artifact struct {
	AttemptID string `json:"attempt_id"`
	Name      string `json:"name"`
	URI       string `json:"uri"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	// Content is the stored artifact text when the worker inlined it in its
	// announcement; empty when only metadata was announced.
	Content   string    `json:"content,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type WebhookDeliveryStatus struct {
	ID           string     `json:"id"`
	State        string     `json:"state"`
	AttemptCount uint32     `json:"attempt_count"`
	ResponseCode *int       `json:"response_code,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	DeliveredAt  *time.Time `json:"delivered_at,omitempty"`
}

type HealthFiring struct {
	ID          string                `json:"id"`
	AttemptID   string                `json:"attempt_id"`
	PolicyIndex uint32                `json:"policy_index"`
	Kind        string                `json:"kind"`
	Target      string                `json:"target"`
	Reason      string                `json:"reason"`
	CreatedAt   time.Time             `json:"created_at"`
	Delivery    WebhookDeliveryStatus `json:"delivery"`
}

type JobDetail struct {
	Job       Job              `json:"job"`
	Attempts  []Attempt        `json:"attempts"`
	Metrics   []RecordedMetric `json:"metrics"`
	Artifacts []Artifact       `json:"artifacts"`
	Health    []HealthFiring   `json:"health"`
}

type WorkerEvent struct {
	Sequence   uint64                 `json:"sequence"`
	EventID    string                 `json:"event_id"`
	AttemptID  string                 `json:"attempt_id"`
	Kind       WorkerEventKind        `json:"kind"`
	OccurredAt time.Time              `json:"occurred_at"`
	ExitCode   *int                   `json:"exit_code,omitempty"`
	Error      string                 `json:"error,omitempty"`
	LogURI     string                 `json:"log_uri,omitempty"`
	LogTail    string                 `json:"log_tail,omitempty"`
	Git        *GitState              `json:"git,omitempty"`
	Metric     *MetricSample          `json:"metric,omitempty"`
	Artifacts  []ArtifactAnnouncement `json:"artifacts,omitempty"`
}

type WorkerSyncRequest struct {
	WorkerID     string         `json:"worker_id"`
	SessionID    string         `json:"session_id"`
	AgentVersion string         `json:"agent_version"`
	Capacity     WorkerCapacity `json:"capacity"`
	Events       []WorkerEvent  `json:"events,omitempty"`
}

type WorkerCommand struct {
	CommandID      string    `json:"command_id"`
	AttemptID      string    `json:"attempt_id"`
	AttemptNumber  uint32    `json:"attempt_number"`
	Kind           string    `json:"kind"`
	Job            Job       `json:"job"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type WorkerSyncResponse struct {
	ServerTime      time.Time       `json:"server_time"`
	AcceptedThrough uint64          `json:"accepted_through"`
	NextSyncSeconds uint32          `json:"next_sync_seconds"`
	Commands        []WorkerCommand `json:"commands,omitempty"`
}
