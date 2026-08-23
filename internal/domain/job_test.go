package domain

import (
	"strings"
	"testing"
)

func validSpec() JobSpec {
	return JobSpec{
		Project: "example-research",
		Name:    "train-baseline",
		Source: Source{
			GitURL: "https://github.com/example/research.git",
			Commit: strings.Repeat("a", 40),
		},
		Command: []string{"python", "train.py"},
		Resources: ResourceRequest{GPUs: []GPURequest{{
			Count:   1,
			Sharing: GPUExclusive,
		}}},
		Retry: RetryPolicy{MaxAttempts: 1, LostWorker: LostWorkerManual},
	}
}

func TestJobSpecValidate(t *testing.T) {
	spec := validSpec()
	precision := uint8(1)
	spec.Metrics = []MetricDefinition{{
		Name: "validation/accuracy", DisplayName: "Accuracy",
		Format: MetricFormatPercent, Precision: &precision,
		Objective: MetricObjectiveMaximize,
		ReferenceLines: []MetricReferenceLine{
			{Label: "Human best", Value: 0.945, Kind: MetricReferenceBenchmark},
			{Label: "Goal", Value: 0.96, Kind: MetricReferenceGoal},
		},
	}}
	spec.Health = []HealthPolicy{{
		Kind: HealthKindMetricStalled, Metric: "validation/accuracy", Mode: HealthModeMax,
		Window: "30m", MinimumDelta: 0.001, Action: HealthActionNotify, Target: "research-session",
	}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
}

func TestJobSpecValidateRejectsInvalidHealthPolicy(t *testing.T) {
	spec := validSpec()
	spec.Health = []HealthPolicy{{
		Kind: HealthKindMetricStalled, Metric: "missing", Mode: "up", Window: "2s",
		MinimumDelta: -1, Action: "cancel", Target: "Not A Slug",
	}}
	err := spec.Validate()
	if err == nil {
		t.Fatal("expected health policy validation error")
	}
	for _, want := range []string{"duration", "action", "target", "declared metric", "mode", "minimum_delta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestJobSpecValidateRejectsAmbiguousSourceAndReservedEnvironment(t *testing.T) {
	spec := validSpec()
	spec.Source.OCIImage = "registry.example.com/research@sha256:abc"
	spec.Environment = map[string]string{"LCJS_TOKEN": "do-not-store-this"}

	err := spec.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"exactly one", "reserved LCJS_"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestJobSpecValidateRejectsInvalidMetricDefinitions(t *testing.T) {
	spec := validSpec()
	tooPrecise := uint8(10)
	spec.Metrics = []MetricDefinition{
		{
			Name: "accuracy", Format: MetricFormatPercent, Unit: "%", Precision: &tooPrecise,
			Objective: "largest",
			ReferenceLines: []MetricReferenceLine{
				{Label: "Human best", Value: 94.5, Kind: MetricReferenceBenchmark},
				{Label: "human BEST", Value: 0.95, Kind: "comparison"},
			},
		},
		{Name: "accuracy"},
	}

	err := spec.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"duplicated", "unit must be empty", "precision", "objective", "between 0 and 1", "kind must be"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestJobSpecValidateReservesSystemMetricPrefixAndAcceptsBytes(t *testing.T) {
	spec := validSpec()
	spec.Metrics = []MetricDefinition{{Name: SystemMetricPrefix + "rss_bytes"}}
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved prefix error = %v", err)
	}

	spec.Metrics = []MetricDefinition{{Name: "memory/peak", Format: MetricFormatBytes}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("bytes metric: %v", err)
	}
	spec.Metrics[0].Unit = "MiB"
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "unit must be empty") {
		t.Fatalf("bytes unit error = %v", err)
	}

	for _, definition := range SystemMetricDefinitions() {
		if !strings.HasPrefix(definition.Name, SystemMetricPrefix) || definition.DisplayName == "" {
			t.Fatalf("system metric definition = %+v", definition)
		}
	}
}

func TestJobSpecValidateRejectsArtifactPathEscape(t *testing.T) {
	spec := validSpec()
	spec.Artifacts = []ArtifactRule{{Name: "results", Glob: "../private/*"}}
	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "inside the job work directory") {
		t.Fatalf("error = %v, want artifact path rejection", err)
	}
}
