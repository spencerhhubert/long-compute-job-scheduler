package httpapi

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

// The report endpoint answers one question: what is going on with this job,
// in a form something can read without making six more calls and doing the
// arithmetic itself.
//
// It returns markdown rather than JSON on purpose. A poller that checks on a
// long run every fifteen minutes does not want a metric series, it wants to be
// told what changed, how the series is moving, how far it is from the numbers
// the spec said mattered, and what the run has written out. Everything here is
// derived from data the API already exposes; the value is entirely in the
// aggregation and the framing.
//
// Aggregation is at three levels for every series: the whole run, the recent
// window, and the last sample. Every reference line declared in the job spec
// is turned into a distance, signed so that positive always means "better than
// this line" given the metric's own objective.

const (
	reportRecentWindow = 10
	reportInlineBytes  = 8000
	reportLogTailBytes = 2000
)

func (s *Server) jobReport(response http.ResponseWriter, request *http.Request) {
	detail, err := s.store.GetJobDetail(request.Context(), request.PathValue("id"))
	if errors.Is(err, sqlitestore.ErrNotFound) {
		writeError(response, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "the job could not be read")
		return
	}
	full := request.URL.Query().Get("artifacts") != "none"
	body := renderJobReport(detail, time.Now().UTC(), full)
	response.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(body))
}

// projectReport summarises every job in a project that is worth looking at:
// anything running or queued, plus whatever finished most recently.
func (s *Server) projectReport(response http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	limit := 6
	if raw := request.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 50 {
			limit = parsed
		}
	}
	jobs, err := s.store.ListJobs(request.Context(), 200)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "jobs could not be listed")
		return
	}
	var picked []domain.Job
	for _, job := range jobs {
		if project != "" && job.Spec.Project != project {
			continue
		}
		picked = append(picked, job)
		if len(picked) >= limit {
			break
		}
	}
	now := time.Now().UTC()
	var out strings.Builder
	title := "all projects"
	if project != "" {
		title = project
	}
	fmt.Fprintf(&out, "# %s: %d job(s), %s\n\n", title, len(picked), now.Format(time.RFC3339))
	var active, done []string
	for _, job := range picked {
		line := fmt.Sprintf("- `%s` **%s** (%s, updated %s ago)",
			job.Spec.Name, job.State, job.ID, humanDuration(now.Sub(job.UpdatedAt)))
		if job.State == domain.JobRunning || job.State == domain.JobQueued {
			active = append(active, line)
		} else {
			done = append(done, line)
		}
	}
	if len(active) > 0 {
		out.WriteString("## Active\n\n" + strings.Join(active, "\n") + "\n\n")
	}
	if len(done) > 0 {
		out.WriteString("## Recently finished\n\n" + strings.Join(done, "\n") + "\n\n")
	}
	for _, job := range picked {
		if job.State != domain.JobRunning {
			continue
		}
		detail, err := s.store.GetJobDetail(request.Context(), job.ID)
		if err != nil {
			continue
		}
		out.WriteString("\n---\n\n")
		out.WriteString(renderJobReport(detail, now, false))
	}
	response.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(out.String()))
}

type seriesSummary struct {
	def       domain.MetricDefinition
	points    []domain.RecordedMetric
	first     float64
	last      float64
	min       float64
	max       float64
	mean      float64
	recentAvg float64
	priorAvg  float64
}

func renderJobReport(detail domain.JobDetail, now time.Time, withArtifacts bool) string {
	var out strings.Builder
	job := detail.Job

	fmt.Fprintf(&out, "# %s — %s\n\n", job.Spec.Name, strings.ToUpper(string(job.State)))
	fmt.Fprintf(&out, "`%s` in project `%s`, created %s ago, updated %s ago.\n\n",
		job.ID, job.Spec.Project, humanDuration(now.Sub(job.CreatedAt)),
		humanDuration(now.Sub(job.UpdatedAt)))
	fmt.Fprintf(&out, "Command: `%s`\n\n", strings.Join(job.Spec.Command, " "))

	writeAttempts(&out, detail.Attempts, now)
	writeMetrics(&out, job.Spec.Metrics, detail.Metrics)
	writeResources(&out, detail.Metrics)
	writeArtifacts(&out, detail.Artifacts, withArtifacts)
	writeLogTail(&out, detail.Attempts)
	return out.String()
}

func writeAttempts(out *strings.Builder, attempts []domain.Attempt, now time.Time) {
	if len(attempts) == 0 {
		out.WriteString("No attempt has started yet. If this persists while the GPU is idle, " +
			"check that the spec's resource request fits the worker's advertised capacity.\n\n")
		return
	}
	out.WriteString("## Attempts\n\n```\n")
	for _, a := range attempts {
		elapsed := "—"
		if a.StartedAt != nil {
			end := now
			if a.FinishedAt != nil {
				end = *a.FinishedAt
			}
			elapsed = humanDuration(end.Sub(*a.StartedAt))
		}
		exit := "—"
		if a.ExitCode != nil {
			exit = strconv.Itoa(*a.ExitCode)
		}
		fmt.Fprintf(out, "#%d %-10s worker=%s elapsed=%-8s exit=%s %s\n",
			a.AttemptNumber, a.State, shortID(a.WorkerID), elapsed, exit, a.Error)
	}
	out.WriteString("```\n\n")
}

func writeMetrics(out *strings.Builder, defs []domain.MetricDefinition, recorded []domain.RecordedMetric) {
	byName := map[string][]domain.RecordedMetric{}
	for _, m := range recorded {
		if strings.HasPrefix(m.Name, "lcjs/") {
			continue
		}
		byName[m.Name] = append(byName[m.Name], m)
	}
	if len(byName) == 0 {
		out.WriteString("## Metrics\n\nNothing reported yet.\n\n")
		return
	}
	defByName := map[string]domain.MetricDefinition{}
	for _, d := range defs {
		defByName[d.Name] = d
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	out.WriteString("## Metrics\n\n")
	out.WriteString("Each row is the whole run, then the last " +
		strconv.Itoa(reportRecentWindow) + " samples, then the trend between them.\n\n```\n")
	fmt.Fprintf(out, "%-38s %5s %10s %10s %10s %10s %9s\n",
		"metric", "n", "first", "last", "best", "recent", "trend")
	for _, name := range names {
		s := summarise(defByName[name], byName[name])
		fmt.Fprintf(out, "%-38s %5d %10s %10s %10s %10s %9s\n",
			truncate(name, 38), len(s.points), num(s.first), num(s.last),
			num(s.best()), num(s.recentAvg), s.trend())
	}
	out.WriteString("```\n\n")

	for _, name := range names {
		def, ok := defByName[name]
		if !ok || len(def.ReferenceLines) == 0 {
			continue
		}
		s := summarise(def, byName[name])
		label := def.DisplayName
		if label == "" {
			label = name
		}
		fmt.Fprintf(out, "**%s** is at %s. Against the lines the spec declared:\n\n```\n",
			label, num(s.last))
		for _, line := range def.ReferenceLines {
			gap := s.last - line.Value
			verb := "above"
			if gap < 0 {
				verb = "below"
				gap = -gap
			}
			beating := s.beats(line.Value)
			mark := "still behind"
			if beating {
				mark = "PASSED"
			}
			fmt.Fprintf(out, "%-52s %10s  %s by %-10s %s\n",
				truncate(line.Label, 52), num(line.Value), verb, num(gap), mark)
		}
		out.WriteString("```\n\n")
	}
}

func writeResources(out *strings.Builder, recorded []domain.RecordedMetric) {
	byName := map[string][]float64{}
	for _, m := range recorded {
		if strings.HasPrefix(m.Name, "lcjs/") {
			byName[m.Name] = append(byName[m.Name], m.Value)
		}
	}
	if len(byName) == 0 {
		return
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out.WriteString("## Resources\n\n```\n")
	for _, name := range names {
		v := byName[name]
		sorted := append([]float64(nil), v...)
		sort.Float64s(sorted)
		p90 := sorted[(len(sorted)*9)/10]
		if len(sorted) == 1 {
			p90 = sorted[0]
		}
		fmt.Fprintf(out, "%-24s n=%-5d mean=%-12s p90=%-12s max=%s\n",
			strings.TrimPrefix(name, "lcjs/"), len(v),
			num(mean(v)), num(p90), num(sorted[len(sorted)-1]))
	}
	out.WriteString("```\n\n")
}

func writeArtifacts(out *strings.Builder, artifacts []domain.Artifact, inline bool) {
	if len(artifacts) == 0 {
		out.WriteString("## Artifacts\n\nNone recorded yet.\n\n")
		return
	}
	out.WriteString("## Artifacts\n\n```\n")
	for _, a := range artifacts {
		fmt.Fprintf(out, "%-22s %9d bytes  %s\n", a.Name, a.SizeBytes,
			a.CreatedAt.Format(time.RFC3339))
	}
	out.WriteString("```\n\n")
	if !inline {
		return
	}
	for _, a := range artifacts {
		if a.Content == "" {
			continue
		}
		body := a.Content
		note := ""
		if len(body) > reportInlineBytes {
			body = body[:reportInlineBytes]
			note = fmt.Sprintf("\n... truncated at %d of %d bytes\n", reportInlineBytes, len(a.Content))
		}
		fmt.Fprintf(out, "### %s\n\n```\n%s%s\n```\n\n", a.Name, body, note)
	}
}

func writeLogTail(out *strings.Builder, attempts []domain.Attempt) {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].LogTail == "" {
			continue
		}
		tail := attempts[i].LogTail
		if len(tail) > reportLogTailBytes {
			tail = tail[len(tail)-reportLogTailBytes:]
		}
		fmt.Fprintf(out, "## Log tail (attempt %d)\n\n```\n%s\n```\n\n",
			attempts[i].AttemptNumber, tail)
		return
	}
	out.WriteString("## Log tail\n\nNot available while the attempt is still running; " +
		"the worker reports it when the attempt ends.\n\n")
}

func summarise(def domain.MetricDefinition, points []domain.RecordedMetric) seriesSummary {
	sort.SliceStable(points, func(i, j int) bool {
		return points[i].ObservedAt.Before(points[j].ObservedAt)
	})
	s := seriesSummary{def: def, points: points}
	values := make([]float64, len(points))
	for i, p := range points {
		values[i] = p.Value
	}
	s.first, s.last = values[0], values[len(values)-1]
	s.min, s.max = values[0], values[0]
	for _, v := range values {
		s.min = math.Min(s.min, v)
		s.max = math.Max(s.max, v)
	}
	s.mean = mean(values)
	cut := len(values) - reportRecentWindow
	if cut < 0 {
		cut = 0
	}
	s.recentAvg = mean(values[cut:])
	if cut > 0 {
		s.priorAvg = mean(values[:cut])
	} else {
		s.priorAvg = s.recentAvg
	}
	return s
}

func (s seriesSummary) best() float64 {
	if s.def.Objective == domain.MetricObjectiveMaximize {
		return s.max
	}
	return s.min
}

// beats reports whether the latest value is on the good side of a line, in the
// direction the metric's own objective says is good.
func (s seriesSummary) beats(line float64) bool {
	if s.def.Objective == domain.MetricObjectiveMaximize {
		return s.last >= line
	}
	return s.last <= line
}

func (s seriesSummary) trend() string {
	delta := s.recentAvg - s.priorAvg
	if math.Abs(delta) < 1e-12 {
		return "flat"
	}
	improving := delta < 0
	if s.def.Objective == domain.MetricObjectiveMaximize {
		improving = delta > 0
	}
	if s.def.Objective == "" || s.def.Objective == domain.MetricObjectiveNone {
		if delta > 0 {
			return "rising"
		}
		return "falling"
	}
	if improving {
		return "improving"
	}
	return "worsening"
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	total := 0.0
	for _, x := range v {
		total += x
	}
	return total / float64(len(v))
}

func num(v float64) string {
	switch {
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case math.Abs(v) >= 1e6 || (math.Abs(v) < 1e-4 && v != 0):
		return strconv.FormatFloat(v, 'g', 4, 64)
	default:
		return strconv.FormatFloat(v, 'f', 4, 64)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
