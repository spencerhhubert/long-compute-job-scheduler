package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func precisionOf(v uint8) *uint8 { return &v }

func sampleDetail() domain.JobDetail {
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	started := base
	exit := 0
	return domain.JobDetail{
		Job: domain.Job{
			ID:    "job_report_fixture",
			State: domain.JobRunning,
			Spec: domain.JobSpec{
				Project: "compression-challenge-01",
				Name:    "cm-prog-seed0",
				Command: []string{"python3", "-m", "cm.psearch"},
				Metrics: []domain.MetricDefinition{{
					Name:        "search/best_bits",
					DisplayName: "Best program found",
					Objective:   domain.MetricObjectiveMinimize,
					Precision:   precisionOf(4),
					ReferenceLines: []domain.MetricReferenceLine{
						{Label: "hand-written stack", Value: 4.8208, Kind: "baseline"},
						{Label: "best eval.sh entry", Value: 4.79, Kind: "goal"},
					},
				}},
			},
			CreatedAt: base,
			UpdatedAt: base.Add(30 * time.Minute),
		},
		Attempts: []domain.Attempt{{
			AttemptNumber: 1, State: domain.AttemptRunning, WorkerID: "wrk_0gq240sbkq68x4m39h8wkf9s8m",
			StartedAt: &started, ExitCode: &exit, LogTail: "iter 41 best 4.8084",
		}},
		Metrics: []domain.RecordedMetric{
			{Name: "search/best_bits", Value: 4.8181, ObservedAt: base.Add(1 * time.Minute)},
			{Name: "search/best_bits", Value: 4.8120, ObservedAt: base.Add(2 * time.Minute)},
			{Name: "search/best_bits", Value: 4.8084, ObservedAt: base.Add(3 * time.Minute)},
			{Name: "lcjs/gpu_util", Value: 0.99, ObservedAt: base.Add(1 * time.Minute)},
			{Name: "lcjs/gpu_util", Value: 1.00, ObservedAt: base.Add(2 * time.Minute)},
		},
		Artifacts: []domain.Artifact{{
			Name: "champion-bqn", SizeBytes: 42, CreatedAt: base.Add(3 * time.Minute),
			Content: "Contexts ← ⟨{𝕊 S‿R: 4096|(1»S)}⟩",
		}},
	}
}

func TestReportSummarisesASeriesAtThreeLevels(t *testing.T) {
	out := renderJobReport(sampleDetail(), time.Date(2026, 8, 23, 12, 30, 0, 0, time.UTC), true)
	for _, want := range []string{"cm-prog-seed0", "RUNNING", "search/best_bits", "4.8181", "4.8084"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestReportOrientsEveryReferenceLineByTheMetricsObjective(t *testing.T) {
	out := renderJobReport(sampleDetail(), time.Now().UTC(), true)
	// 4.8084 beats the 4.8208 baseline on a minimise metric, and does not yet
	// reach the 4.79 goal. Both statements have to survive the sign handling.
	if !strings.Contains(out, "PASSED") {
		t.Fatalf("a passed reference line was not marked:\n%s", out)
	}
	if !strings.Contains(out, "still behind") {
		t.Fatalf("an unmet reference line was not marked:\n%s", out)
	}
}

func TestReportSeparatesWorkerTelemetryFromTheJobsOwnMetrics(t *testing.T) {
	out := renderJobReport(sampleDetail(), time.Now().UTC(), true)
	metrics := out[strings.Index(out, "## Metrics"):strings.Index(out, "## Resources")]
	if strings.Contains(metrics, "gpu_util") {
		t.Fatalf("worker telemetry leaked into the metrics table:\n%s", metrics)
	}
	if !strings.Contains(out, "gpu_util") {
		t.Fatalf("worker telemetry is missing from the resources section:\n%s", out)
	}
}

func TestReportInlinesSmallTextArtifacts(t *testing.T) {
	out := renderJobReport(sampleDetail(), time.Now().UTC(), true)
	if !strings.Contains(out, "Contexts ←") {
		t.Fatalf("artifact content was not inlined:\n%s", out)
	}
	short := renderJobReport(sampleDetail(), time.Now().UTC(), false)
	if strings.Contains(short, "Contexts ←") {
		t.Fatalf("artifact content was inlined when it was not asked for")
	}
}

func TestReportExplainsAnUnscheduledJob(t *testing.T) {
	detail := sampleDetail()
	detail.Attempts = nil
	out := renderJobReport(detail, time.Now().UTC(), false)
	if !strings.Contains(out, "advertised capacity") {
		t.Fatalf("a job with no attempt should say why that happens:\n%s", out)
	}
}

func TestTrendReadsInTheDirectionTheMetricCaresAbout(t *testing.T) {
	falling := []domain.RecordedMetric{}
	base := time.Now().UTC()
	for i := 0; i < 20; i++ {
		falling = append(falling, domain.RecordedMetric{
			Value: 5.0 - float64(i)*0.01, ObservedAt: base.Add(time.Duration(i) * time.Minute)})
	}
	min := summarise(domain.MetricDefinition{Objective: domain.MetricObjectiveMinimize}, falling)
	max := summarise(domain.MetricDefinition{Objective: domain.MetricObjectiveMaximize}, falling)
	if min.trend() != "improving" {
		t.Fatalf("a falling minimise metric should be improving, got %q", min.trend())
	}
	if max.trend() != "worsening" {
		t.Fatalf("a falling maximise metric should be worsening, got %q", max.trend())
	}
}

func TestProjectReportInlinesArtifactsForRunningJobs(t *testing.T) {
	// The project view is what a periodic poller reads, so it has to carry
	// the run's actual output. Reporting only names and sizes there was the
	// difference between watching a search and watching a directory listing.
	detail := sampleDetail()
	out := renderJobReport(detail, time.Now().UTC(), true)
	if !strings.Contains(out, "Contexts ←") {
		t.Fatalf("running job report must inline artifact content:\n%s", out)
	}
}
