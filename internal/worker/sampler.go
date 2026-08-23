package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

// resourceSampleInterval paces the automatic per-attempt resource series (see
// domain.SystemMetricDefinitions). Samples are appended to the attempt's
// metrics file, so they ride the existing durable ingest and sync path.
const resourceSampleInterval = 15 * time.Second

func (a *Agent) ensureSampler(command StoredCommand) {
	if _, running := a.samplers[command.Command.CommandID]; running || command.PID <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.samplers[command.Command.CommandID] = cancel
	go sampleResources(ctx, command.PID, command.MetricsPath, len(command.Command.Job.Spec.Resources.GPUs) > 0)
}

func (a *Agent) stopSampler(commandID string) {
	if cancel, running := a.samplers[commandID]; running {
		cancel()
		delete(a.samplers, commandID)
	}
}

// sampleResources reports resource telemetry until the attempt ends. It is
// best effort by design: a failing probe is logged and skipped, never allowed
// to fail the attempt. GPU probes stop after the first nvidia-smi failure.
func sampleResources(ctx context.Context, processGroup int, metricsPath string, wantGPU bool) {
	ticker := time.NewTicker(resourceSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now().UTC()
		samples := make([]domain.MetricSample, 0, 3)
		if rss, err := processGroupRSS(processGroup); err == nil {
			samples = append(samples, domain.MetricSample{Name: domain.SystemMetricPrefix + "rss_bytes", Value: float64(rss), ObservedAt: now})
		}
		if wantGPU {
			utilization, memoryBytes, err := queryGPU(ctx)
			if err != nil {
				slog.Warn("gpu resource sampling disabled for this attempt", "error", err)
				wantGPU = false
			} else {
				samples = append(samples,
					domain.MetricSample{Name: domain.SystemMetricPrefix + "gpu_util", Value: utilization, ObservedAt: now},
					domain.MetricSample{Name: domain.SystemMetricPrefix + "gpu_mem_bytes", Value: memoryBytes, ObservedAt: now})
			}
		}
		if err := appendSamples(metricsPath, samples); err != nil {
			slog.Warn("resource samples could not be appended", "error", err)
		}
	}
}

// processGroupRSS sums resident set bytes across the attempt's process group;
// the supervisor starts every attempt in its own group.
func processGroupRSS(processGroup int) (uint64, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	pageSize := uint64(os.Getpagesize())
	group := strconv.Itoa(processGroup)
	var total uint64
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// Fields after the parenthesized command name: state ppid pgrp ...
		// with rss in pages at overall field 24 (man 5 proc).
		_, after, found := strings.Cut(string(data), ") ")
		fields := strings.Fields(after)
		if !found || len(fields) < 22 || fields[2] != group {
			continue
		}
		if pages, err := strconv.ParseUint(fields[21], 10, 64); err == nil {
			total += pages * pageSize
		}
	}
	return total, nil
}

func queryGPU(ctx context.Context) (float64, float64, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(queryCtx, "nvidia-smi", "--query-gpu=utilization.gpu,memory.used", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseGPUQuery(string(output))
}

// parseGPUQuery reduces one "utilization, memory-used-MiB" line per GPU to a
// mean utilization fraction and total used memory in bytes.
func parseGPUQuery(output string) (float64, float64, error) {
	var utilizationSum, memoryBytes float64
	var gpus int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		utilizationText, memoryText, found := strings.Cut(line, ",")
		utilization, utilizationErr := strconv.ParseFloat(strings.TrimSpace(utilizationText), 64)
		memoryMiB, memoryErr := strconv.ParseFloat(strings.TrimSpace(memoryText), 64)
		if !found || utilizationErr != nil || memoryErr != nil {
			return 0, 0, fmt.Errorf("unexpected nvidia-smi output %q", strings.TrimSpace(line))
		}
		utilizationSum += utilization / 100
		memoryBytes += memoryMiB * (1 << 20)
		gpus++
	}
	if gpus == 0 {
		return 0, 0, errors.New("nvidia-smi reported no GPUs")
	}
	return utilizationSum / float64(gpus), memoryBytes, nil
}

func appendSamples(path string, samples []domain.MetricSample) error {
	if len(samples) == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var encodeErr error
	encoder := json.NewEncoder(file)
	for _, sample := range samples {
		encodeErr = errors.Join(encodeErr, encoder.Encode(sample))
	}
	return errors.Join(encodeErr, file.Close())
}
