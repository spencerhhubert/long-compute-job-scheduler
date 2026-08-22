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
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
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
