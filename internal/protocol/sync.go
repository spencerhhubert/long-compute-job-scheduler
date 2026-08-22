// Package protocol defines versioned messages exchanged between workers and the
// control plane. It deliberately contains no HTTP or persistence behavior.
package protocol

import (
	"encoding/json"
	"time"
)

type ResourceSnapshot struct {
	ObservedAt       time.Time         `json:"observed_at"`
	CPUTotal         uint32            `json:"cpu_total"`
	MemoryBytesTotal uint64            `json:"memory_bytes_total"`
	ScratchBytesFree uint64            `json:"scratch_bytes_free"`
	Labels           map[string]string `json:"labels,omitempty"`
	GPUs             []GPU             `json:"gpus,omitempty"`
}

type GPU struct {
	ID           string   `json:"id"`
	Model        string   `json:"model"`
	MemoryBytes  uint64   `json:"memory_bytes"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type Lease struct {
	AttemptID string `json:"attempt_id"`
	Revision  uint64 `json:"revision"`
}

type Event struct {
	Sequence   uint64          `json:"sequence"`
	EventID    string          `json:"event_id"`
	AttemptID  string          `json:"attempt_id,omitempty"`
	Kind       string          `json:"kind"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

type SyncRequest struct {
	WorkerID     string           `json:"worker_id"`
	SessionID    string           `json:"session_id"`
	AgentVersion string           `json:"agent_version"`
	Resources    ResourceSnapshot `json:"resources"`
	Leases       []Lease          `json:"leases,omitempty"`
	Events       []Event          `json:"events,omitempty"`
}

type Command struct {
	CommandID      string          `json:"command_id"`
	Kind           string          `json:"kind"`
	AttemptID      string          `json:"attempt_id,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

type SyncResponse struct {
	ServerTime      time.Time `json:"server_time"`
	AcceptedThrough uint64    `json:"accepted_through"`
	NextSyncAfter   string    `json:"next_sync_after"`
	Commands        []Command `json:"commands,omitempty"`
}
