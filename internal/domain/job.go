// Package domain contains the scheduler's storage- and transport-independent
// model.
package domain

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
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
	Target       string  `json:"target"`
}

const (
	HealthKindMetricStalled = "metric_stalled"
	HealthKindPeriodic      = "periodic"
	HealthModeMax           = "max"
	HealthModeMin           = "min"
	HealthActionNotify      = "notify"
)

type MetricFormat string

const (
	MetricFormatNumber  MetricFormat = "number"
	MetricFormatPercent MetricFormat = "percent"
)

type MetricObjective string

const (
	MetricObjectiveNone     MetricObjective = "none"
	MetricObjectiveMaximize MetricObjective = "maximize"
	MetricObjectiveMinimize MetricObjective = "minimize"
)

type MetricReferenceKind string

const (
	MetricReferenceGoal      MetricReferenceKind = "goal"
	MetricReferenceBenchmark MetricReferenceKind = "benchmark"
	MetricReferenceBaseline  MetricReferenceKind = "baseline"
	MetricReferenceThreshold MetricReferenceKind = "threshold"
)

// MetricReferenceLine is a named horizontal annotation on a metric chart. It
// is descriptive; actions triggered by metric values belong in HealthPolicy.
type MetricReferenceLine struct {
	Label string              `json:"label"`
	Value float64             `json:"value"`
	Kind  MetricReferenceKind `json:"kind"`
}

// MetricDefinition describes how one scalar series should be presented and
// compared. Percent values use a raw scale from 0 through 1.
type MetricDefinition struct {
	Name           string                `json:"name"`
	DisplayName    string                `json:"display_name,omitempty"`
	Format         MetricFormat          `json:"format,omitempty"`
	Unit           string                `json:"unit,omitempty"`
	Precision      *uint8                `json:"precision,omitempty"`
	Objective      MetricObjective       `json:"objective,omitempty"`
	ReferenceLines []MetricReferenceLine `json:"reference_lines,omitempty"`
}

type JobSpec struct {
	Project     string             `json:"project"`
	Name        string             `json:"name"`
	Priority    int32              `json:"priority"`
	Source      Source             `json:"source"`
	Command     []string           `json:"command"`
	Environment map[string]string  `json:"environment,omitempty"`
	SecretRefs  map[string]string  `json:"secret_refs,omitempty"`
	Resources   ResourceRequest    `json:"resources"`
	Constraints Constraints        `json:"constraints,omitempty"`
	Retry       RetryPolicy        `json:"retry"`
	Checkpoint  *CheckpointPolicy  `json:"checkpoint,omitempty"`
	Artifacts   []ArtifactRule     `json:"artifacts,omitempty"`
	Metrics     []MetricDefinition `json:"metrics,omitempty"`
	Health      []HealthPolicy     `json:"health,omitempty"`
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
var metricPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,126}[A-Za-z0-9]$|^[A-Za-z0-9]$`)

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
		} else if unsafeRelativeGlob(spec.Checkpoint.Glob) {
			errs = append(errs, errors.New("checkpoint glob must remain inside the job work directory"))
		}
	}
	for i, artifact := range spec.Artifacts {
		if !slugPattern.MatchString(artifact.Name) || strings.TrimSpace(artifact.Glob) == "" {
			errs = append(errs, fmt.Errorf("artifacts[%d] requires a slug name and non-empty glob", i))
		} else if unsafeRelativeGlob(artifact.Glob) {
			errs = append(errs, fmt.Errorf("artifacts[%d].glob must remain inside the job work directory", i))
		}
	}
	if len(spec.Metrics) > 64 {
		errs = append(errs, errors.New("metrics may contain at most 64 definitions"))
	}
	metricNames := make(map[string]struct{}, len(spec.Metrics))
	for i, metric := range spec.Metrics {
		path := fmt.Sprintf("metrics[%d]", i)
		if !metricPattern.MatchString(metric.Name) {
			errs = append(errs, fmt.Errorf("%s.name must contain 1 to 128 letters, numbers, dots, underscores, slashes, or hyphens", path))
		} else if _, exists := metricNames[metric.Name]; exists {
			errs = append(errs, fmt.Errorf("%s.name %q is duplicated", path, metric.Name))
		} else {
			metricNames[metric.Name] = struct{}{}
		}
		if len(metric.DisplayName) > 100 {
			errs = append(errs, fmt.Errorf("%s.display_name must be at most 100 characters", path))
		}
		format := metric.Format
		if format == "" {
			format = MetricFormatNumber
		}
		if format != MetricFormatNumber && format != MetricFormatPercent {
			errs = append(errs, fmt.Errorf("%s.format must be %q or %q", path, MetricFormatNumber, MetricFormatPercent))
		}
		if len(metric.Unit) > 24 {
			errs = append(errs, fmt.Errorf("%s.unit must be at most 24 characters", path))
		}
		if format == MetricFormatPercent && metric.Unit != "" {
			errs = append(errs, fmt.Errorf("%s.unit must be empty when format is %q", path, MetricFormatPercent))
		}
		if metric.Precision != nil && *metric.Precision > 9 {
			errs = append(errs, fmt.Errorf("%s.precision must be between 0 and 9", path))
		}
		objective := metric.Objective
		if objective == "" {
			objective = MetricObjectiveNone
		}
		if objective != MetricObjectiveNone && objective != MetricObjectiveMaximize && objective != MetricObjectiveMinimize {
			errs = append(errs, fmt.Errorf("%s.objective must be %q, %q, or %q", path, MetricObjectiveNone, MetricObjectiveMaximize, MetricObjectiveMinimize))
		}
		if len(metric.ReferenceLines) > 16 {
			errs = append(errs, fmt.Errorf("%s.reference_lines may contain at most 16 entries", path))
		}
		lineLabels := make(map[string]struct{}, len(metric.ReferenceLines))
		for lineIndex, line := range metric.ReferenceLines {
			linePath := fmt.Sprintf("%s.reference_lines[%d]", path, lineIndex)
			label := strings.TrimSpace(line.Label)
			if label == "" || len(label) > 100 {
				errs = append(errs, fmt.Errorf("%s.label must contain between 1 and 100 characters", linePath))
			} else if _, exists := lineLabels[strings.ToLower(label)]; exists {
				errs = append(errs, fmt.Errorf("%s.label %q is duplicated", linePath, label))
			} else {
				lineLabels[strings.ToLower(label)] = struct{}{}
			}
			if math.IsNaN(line.Value) || math.IsInf(line.Value, 0) {
				errs = append(errs, fmt.Errorf("%s.value must be finite", linePath))
			}
			if format == MetricFormatPercent && (line.Value < 0 || line.Value > 1) {
				errs = append(errs, fmt.Errorf("%s.value must be between 0 and 1 for percent metrics", linePath))
			}
			if line.Kind != MetricReferenceGoal && line.Kind != MetricReferenceBenchmark && line.Kind != MetricReferenceBaseline && line.Kind != MetricReferenceThreshold {
				errs = append(errs, fmt.Errorf("%s.kind must be %q, %q, %q, or %q", linePath, MetricReferenceGoal, MetricReferenceBenchmark, MetricReferenceBaseline, MetricReferenceThreshold))
			}
		}
	}
	if len(spec.Health) > 16 {
		errs = append(errs, errors.New("health may contain at most 16 policies"))
	}
	for i, policy := range spec.Health {
		path := fmt.Sprintf("health[%d]", i)
		window, err := time.ParseDuration(policy.Window)
		if err != nil || window < 10*time.Second || window > 30*24*time.Hour {
			errs = append(errs, fmt.Errorf("%s.window must be a duration from 10s through 720h", path))
		}
		if policy.Action != HealthActionNotify {
			errs = append(errs, fmt.Errorf("%s.action must currently be %q", path, HealthActionNotify))
		}
		if !slugPattern.MatchString(policy.Target) {
			errs = append(errs, fmt.Errorf("%s.target must be a configured target slug", path))
		}
		switch policy.Kind {
		case HealthKindMetricStalled:
			if _, exists := metricNames[policy.Metric]; !exists {
				errs = append(errs, fmt.Errorf("%s.metric must name a declared metric", path))
			}
			if policy.Mode != HealthModeMax && policy.Mode != HealthModeMin {
				errs = append(errs, fmt.Errorf("%s.mode must be %q or %q", path, HealthModeMax, HealthModeMin))
			}
			if math.IsNaN(policy.MinimumDelta) || math.IsInf(policy.MinimumDelta, 0) || policy.MinimumDelta < 0 {
				errs = append(errs, fmt.Errorf("%s.minimum_delta must be a non-negative finite number", path))
			}
		case HealthKindPeriodic:
			if policy.Metric != "" || policy.Mode != "" || policy.MinimumDelta != 0 {
				errs = append(errs, fmt.Errorf("%s periodic policy may contain only kind, window, action, and target", path))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.kind must be %q or %q", path, HealthKindMetricStalled, HealthKindPeriodic))
		}
	}

	return errors.Join(errs...)
}

func unsafeRelativeGlob(pattern string) bool {
	normalized := filepath.ToSlash(pattern)
	return filepath.IsAbs(pattern) || normalized == ".." || strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../")
}
