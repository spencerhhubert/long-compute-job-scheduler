package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func TestParseGPUQueryReducesMultipleGPUs(t *testing.T) {
	utilization, memoryBytes, err := parseGPUQuery("35, 1024\n65, 2048\n")
	if err != nil {
		t.Fatal(err)
	}
	if utilization != 0.5 {
		t.Fatalf("utilization = %v, want mean fraction 0.5", utilization)
	}
	if memoryBytes != float64(3072)*(1<<20) {
		t.Fatalf("memory = %v bytes, want 3072 MiB", memoryBytes)
	}
	if _, _, err := parseGPUQuery("NVIDIA-SMI has failed"); err == nil {
		t.Fatal("expected an error for unparseable output")
	}
	if _, _, err := parseGPUQuery(""); err == nil {
		t.Fatal("expected an error for empty output")
	}
}

func TestProcessGroupRSSCountsOwnGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("procfs is not available")
	}
	rss, err := processGroupRSS(syscall.Getpgrp())
	if err != nil {
		t.Fatal(err)
	}
	if rss == 0 {
		t.Fatal("resident set for the test's own process group = 0, want > 0")
	}
	if other, err := processGroupRSS(-1); err != nil || other != 0 {
		t.Fatalf("nonexistent group rss = %d, err = %v, want 0", other, err)
	}
}

func TestAppendSamplesWritesMetricsFileLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	samples := []domain.MetricSample{
		{Name: domain.SystemMetricPrefix + "rss_bytes", Value: 42},
		{Name: domain.SystemMetricPrefix + "gpu_util", Value: 0.5},
	}
	if err := appendSamples(path, samples); err != nil {
		t.Fatal(err)
	}
	if err := appendSamples(path, samples[:1]); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3 appended samples", len(lines))
	}
	var decoded domain.MetricSample
	if err := json.Unmarshal([]byte(lines[1]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != domain.SystemMetricPrefix+"gpu_util" || decoded.Value != 0.5 {
		t.Fatalf("decoded sample = %+v", decoded)
	}
	if err := appendSamples(path, nil); err != nil {
		t.Fatal(err)
	}
}
