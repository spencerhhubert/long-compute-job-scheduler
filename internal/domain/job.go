// Package domain contains the scheduler's storage- and transport-independent
// model.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCanceling JobState = "canceling"
	JobCanceled  JobState = "canceled"
	JobRetryWait JobState = "retry_wait"
)

type GPUSharing string

const (
	GPUExclusive GPUSharing = "exclusive"
	GPUShared    GPUSharing = "shared"
)

type LostWorkerAction string

const (
	LostWorkerManual LostWorkerAction = "manual"
	LostWorkerRetry  LostWorkerAction = "retry"
	LostWorkerFail   LostWorkerAction = "fail"
)

type Source struct {
	GitURL   string `json:"git_url,omitempty"`
	Commit   string `json:"commit,omitempty"`
	OCIImage string `json:"oci_image,omitempty"`
}

type GPURequest struct {
	Count       uint32     `json:"count"`
	MemoryBytes uint64     `json:"memory_bytes,omitempty"`
	Sharing     GPUSharing `json:"sharing"`
}

type ResourceRequest struct {
	CPU          uint32       `json:"cpu,omitempty"`
	MemoryBytes  uint64       `json:"memory_bytes,omitempty"`
	ScratchBytes uint64       `json:"scratch_bytes,omitempty"`
	GPUs         []GPURequest `json:"gpus,omitempty"`
}

type Constraints struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts uint32           `json:"max_attempts"`
	LostWorker  LostWorkerAction `json:"lost_worker"`
}

type CheckpointPolicy struct {
	Glob          string   `json:"glob"`
	ResumeCommand []string `json:"resume_command"`
}

type ArtifactRule struct {
	Name string `json:"name"`
	Glob string `json:"glob"`
}

type HealthPolicy struct {
	Kind         string  `json:"kind"`
	Metric       string  `json:"metric,omitempty"`
	Mode         string  `json:"mode,omitempty"`
	Window       string  `json:"window,omitempty"`
	MinimumDelta float64 `json:"minimum_delta,omitempty"`
	Action       string  `json:"action"`
}

type JobSpec struct {
	Project     string            `json:"project"`
	Name        string            `json:"name"`
	Priority    int32             `json:"priority"`
	Source      Source            `json:"source"`
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	SecretRefs  map[string]string `json:"secret_refs,omitempty"`
	Resources   ResourceRequest   `json:"resources"`
	Constraints Constraints       `json:"constraints,omitempty"`
	Retry       RetryPolicy       `json:"retry"`
	Checkpoint  *CheckpointPolicy `json:"checkpoint,omitempty"`
	Artifacts   []ArtifactRule    `json:"artifacts,omitempty"`
	Health      []HealthPolicy    `json:"health,omitempty"`
}

type Job struct {
	ID        string    `json:"id"`
	State     JobState  `json:"state"`
	Revision  uint64    `json:"revision"`
	Spec      JobSpec   `json:"spec"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var commitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (spec JobSpec) Validate() error {
	var errs []error

	if !slugPattern.MatchString(spec.Project) {
		errs = append(errs, errors.New("project must be a lowercase slug of at most 63 characters"))
	}
	if !slugPattern.MatchString(spec.Name) {
		errs = append(errs, errors.New("name must be a lowercase slug of at most 63 characters"))
	}

	hasGit := spec.Source.GitURL != "" || spec.Source.Commit != ""
	hasImage := spec.Source.OCIImage != ""
	if hasGit == hasImage {
		errs = append(errs, errors.New("source must contain exactly one of git revision or OCI image"))
	}
	if hasGit {
		if spec.Source.GitURL == "" {
			errs = append(errs, errors.New("source.git_url is required with source.commit"))
		}
		if !commitPattern.MatchString(spec.Source.Commit) {
			errs = append(errs, errors.New("source.commit must be a full 40- or 64-character hexadecimal object ID"))
		}
	}
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		errs = append(errs, errors.New("command must contain an executable"))
	}

	for name := range spec.Environment {
		if !envPattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("environment key %q is invalid", name))
		}
		if strings.HasPrefix(name, "LCJS_") {
			errs = append(errs, fmt.Errorf("environment key %q uses the reserved LCJS_ prefix", name))
		}
	}
	for name, reference := range spec.SecretRefs {
		if !envPattern.MatchString(name) || strings.TrimSpace(reference) == "" {
			errs = append(errs, fmt.Errorf("secret reference %q is invalid", name))
		}
	}

	for i, gpu := range spec.Resources.GPUs {
		if gpu.Count == 0 {
			errs = append(errs, fmt.Errorf("resources.gpus[%d].count must be greater than zero", i))
		}
		if gpu.Sharing != GPUExclusive && gpu.Sharing != GPUShared {
			errs = append(errs, fmt.Errorf("resources.gpus[%d].sharing must be %q or %q", i, GPUExclusive, GPUShared))
		}
	}

	if spec.Retry.MaxAttempts == 0 {
		errs = append(errs, errors.New("retry.max_attempts must be greater than zero"))
	}
	if spec.Retry.LostWorker != LostWorkerManual && spec.Retry.LostWorker != LostWorkerRetry && spec.Retry.LostWorker != LostWorkerFail {
		errs = append(errs, fmt.Errorf("retry.lost_worker must be %q, %q, or %q", LostWorkerManual, LostWorkerRetry, LostWorkerFail))
	}

	if spec.Checkpoint != nil {
		if strings.TrimSpace(spec.Checkpoint.Glob) == "" || len(spec.Checkpoint.ResumeCommand) == 0 {
			errs = append(errs, errors.New("checkpoint requires a glob and resume_command"))
		}
	}
	for i, artifact := range spec.Artifacts {
		if !slugPattern.MatchString(artifact.Name) || strings.TrimSpace(artifact.Glob) == "" {
			errs = append(errs, fmt.Errorf("artifacts[%d] requires a slug name and non-empty glob", i))
		}
	}

	return errors.Join(errs...)
}
