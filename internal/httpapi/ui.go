package httpapi

import (
	"html/template"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

type loginView struct {
	Error string
}

type dashboardView struct {
	TokenName string
	Jobs      []domain.Job
	Workers   []domain.Worker
	Queued    int
	Active    int
	Failed    int
	Succeeded int
}

type jobDetailView struct {
	TokenName string
	Detail    domain.JobDetail
	Metrics   []metricSeriesView
}

type metricSeriesView struct {
	Definition domain.MetricDefinition
	Samples    []domain.RecordedMetric
	Latest     *domain.RecordedMetric
	References []metricReferenceView
	Points     string
	LatestX    float64
	LatestY    float64
	HasSamples bool
}

type metricReferenceView struct {
	Line domain.MetricReferenceLine
	Y    float64
}

var pageFunctions = template.FuncMap{
	"jobTime": func(value time.Time) string { return value.UTC().Format("2006-01-02 15:04 UTC") },
	"isoTime": func(value time.Time) string { return value.UTC().Format(time.RFC3339) },
	"command": func(parts []string) string {
		if len(parts) == 0 {
			return "—"
		}
		return strings.Join(parts, " ")
	},
	"resources": func(request domain.ResourceRequest) string {
		parts := make([]string, 0, 3)
		if request.CPU > 0 {
			parts = append(parts, formatCount(request.CPU, "CPU"))
		}
		var gpus uint32
		for _, request := range request.GPUs {
			gpus += request.Count
		}
		if gpus > 0 {
			parts = append(parts, formatCount(gpus, "GPU"))
		}
		if len(parts) == 0 {
			return "—"
		}
		return strings.Join(parts, " · ")
	},
	"metricName": func(metric domain.MetricDefinition) string {
		if metric.DisplayName != "" {
			return metric.DisplayName
		}
		return metric.Name
	},
	"metricObjective": func(objective domain.MetricObjective) string {
		switch objective {
		case domain.MetricObjectiveMaximize:
			return "↑ maximize"
		case domain.MetricObjectiveMinimize:
			return "↓ minimize"
		default:
			return ""
		}
	},
	"metricValue": formatMetricValue,
	"labels": func(labels map[string]string) string {
		parts := make([]string, 0, len(labels))
		for name, value := range labels {
			parts = append(parts, name+"="+value)
		}
		sort.Strings(parts)
		if len(parts) == 0 {
			return "—"
		}
		return strings.Join(parts, " · ")
	},
	"workerResources": func(capacity domain.WorkerCapacity) string {
		parts := []string{formatCount(capacity.CPU, "CPU")}
		if capacity.MemoryBytes > 0 {
			parts = append(parts, strconv.FormatUint(capacity.MemoryBytes/(1<<30), 10)+" GiB RAM")
		}
		if capacity.GPUCount > 0 {
			parts = append(parts, formatCount(capacity.GPUCount, "GPU"))
		}
		parts = append(parts, strconv.FormatUint(uint64(capacity.MaxParallel), 10)+" slots")
		return strings.Join(parts, " · ")
	},
	"optionalTime": func(value *time.Time) string {
		if value == nil {
			return "—"
		}
		return value.UTC().Format("2006-01-02 15:04:05 UTC")
	},
	"exitCode": func(value *int) string {
		if value == nil {
			return "—"
		}
		return strconv.Itoa(*value)
	},
}

func buildMetricSeries(detail domain.JobDetail) []metricSeriesView {
	series := make([]metricSeriesView, 0, len(detail.Job.Spec.Metrics))
	for _, definition := range detail.Job.Spec.Metrics {
		view := metricSeriesView{Definition: definition, Samples: make([]domain.RecordedMetric, 0)}
		minimum := math.Inf(1)
		maximum := math.Inf(-1)
		if definition.Format == domain.MetricFormatPercent {
			minimum, maximum = 0, 1
		}
		for _, sample := range detail.Metrics {
			if sample.Name != definition.Name {
				continue
			}
			view.Samples = append(view.Samples, sample)
			minimum = math.Min(minimum, sample.Value)
			maximum = math.Max(maximum, sample.Value)
		}
		for _, line := range definition.ReferenceLines {
			minimum = math.Min(minimum, line.Value)
			maximum = math.Max(maximum, line.Value)
		}
		if math.IsInf(minimum, 1) {
			minimum, maximum = 0, 1
		} else if minimum == maximum {
			padding := math.Abs(minimum) * 0.1
			if padding == 0 {
				padding = 1
			}
			minimum -= padding
			maximum += padding
		} else if definition.Format != domain.MetricFormatPercent {
			padding := (maximum - minimum) * 0.08
			minimum -= padding
			maximum += padding
		}
		yFor := func(value float64) float64 {
			return 36 - ((value-minimum)/(maximum-minimum))*32
		}
		pointParts := make([]string, 0, len(view.Samples))
		for index, sample := range view.Samples {
			x := 50.0
			if len(view.Samples) > 1 {
				x = 4 + (float64(index)/float64(len(view.Samples)-1))*92
			}
			y := yFor(sample.Value)
			pointParts = append(pointParts, strconv.FormatFloat(x, 'f', 2, 64)+","+strconv.FormatFloat(y, 'f', 2, 64))
			view.LatestX, view.LatestY = x, y
		}
		view.Points = strings.Join(pointParts, " ")
		view.HasSamples = len(view.Samples) > 0
		if view.HasSamples {
			latest := view.Samples[len(view.Samples)-1]
			view.Latest = &latest
		}
		for _, line := range definition.ReferenceLines {
			view.References = append(view.References, metricReferenceView{Line: line, Y: yFor(line.Value)})
		}
		series = append(series, view)
	}
	return series
}

func formatCount(count uint32, unit string) string {
	if count == 1 {
		return "1 " + unit
	}
	return strconv.FormatUint(uint64(count), 10) + " " + unit
}

func formatMetricValue(metric domain.MetricDefinition, value float64) string {
	precision := 3
	if metric.Format == domain.MetricFormatPercent {
		precision = 1
		value *= 100
	}
	if metric.Precision != nil {
		precision = int(*metric.Precision)
	}
	formatted := strconv.FormatFloat(value, 'f', precision, 64)
	if metric.Format == domain.MetricFormatPercent {
		return formatted + "%"
	}
	if metric.Unit != "" {
		return formatted + " " + metric.Unit
	}
	return formatted
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en" data-theme="light">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <title>Sign in · Compute Jobs</title>
  <link rel="stylesheet" href="/static/app.css?v=3">
  <script src="/static/theme.js"></script>
</head>
<body>
  <header class="topbar">
    <a class="brand" href="/">Compute Jobs</a>
    <label class="theme-control">Theme
      <select data-theme-picker aria-label="Color theme">
        <option value="light">Light</option>
        <option value="dark">Dark</option>
        <option value="system">System</option>
      </select>
    </label>
  </header>
  <main class="login-main">
    <section class="login-panel" aria-labelledby="login-title">
      <h1 id="login-title">Operator sign in</h1>
      <p>Enter an operator key once. This browser will stay signed in for 90 days.</p>
      {{if .Error}}<div class="notice error" role="alert">{{.Error}}</div>{{end}}
      <form method="post" action="/login">
        <label for="key">Operator key</label>
        <input id="key" name="key" type="password" autocomplete="off" spellcheck="false" required autofocus>
        <button type="submit">Sign in</button>
      </form>
      <p class="fine">The key is exchanged for a secure browser session and is not stored in browser storage.</p>
    </section>
  </main>
</body>
</html>`))

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(pageFunctions).Parse(`<!doctype html>
<html lang="en" data-theme="light">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta http-equiv="refresh" content="30">
  <title>Compute Jobs</title>
  <link rel="stylesheet" href="/static/app.css?v=3">
  <script src="/static/theme.js"></script>
</head>
<body>
  <header class="topbar">
    <a class="brand" href="/">Compute Jobs</a>
    <nav class="toolbar" aria-label="Console controls">
      <span class="online"><span aria-hidden="true"></span>Control plane online</span>
      <label class="theme-control">Theme
        <select data-theme-picker aria-label="Color theme">
          <option value="light">Light</option>
          <option value="dark">Dark</option>
          <option value="system">System</option>
        </select>
      </label>
      <form method="post" action="/logout"><button class="button-secondary" type="submit">Sign out</button></form>
    </nav>
  </header>
  <main class="console">
    <div class="section-heading">
      <div><h1>Overview</h1><p>Central scheduler state · refreshes every 30 seconds</p></div>
      <span class="identity">{{.TokenName}}</span>
    </div>
    <dl class="stats">
      <div><dt>Queued</dt><dd>{{.Queued}}</dd></div>
      <div><dt>Active</dt><dd>{{.Active}}</dd></div>
      <div><dt>Failed</dt><dd>{{.Failed}}</dd></div>
      <div><dt>Succeeded</dt><dd>{{.Succeeded}}</dd></div>
      <div><dt>Workers</dt><dd>{{len .Workers}}</dd></div>
    </dl>

    <section class="panel" aria-labelledby="jobs-title">
      <div class="panel-header"><h2 id="jobs-title">Jobs</h2><span>{{len .Jobs}} total shown</span></div>
      <div class="table-scroll">
        <table>
          <thead><tr><th>State</th><th>Job</th><th>Project</th><th>Resources</th><th>Metrics &amp; references</th><th>Priority</th><th>Created</th></tr></thead>
          <tbody>
            {{range .Jobs}}
            <tr>
              <td><span class="state" data-state="{{.State}}">{{.State}}</span></td>
              <td><strong><a href="/jobs/{{.ID}}">{{.Spec.Name}}</a></strong><small title="{{.ID}}">{{.ID}}</small><small class="command" title="{{command .Spec.Command}}">{{command .Spec.Command}}</small></td>
              <td>{{.Spec.Project}}</td>
              <td>{{resources .Spec.Resources}}</td>
              <td>
                {{if .Spec.Metrics}}<div class="metric-list">
                  {{range $metric := .Spec.Metrics}}<div class="metric-definition">
                    <div><strong>{{metricName $metric}}</strong>{{with metricObjective $metric.Objective}}<small class="objective">{{.}}</small>{{end}}</div>
                    {{range $line := $metric.ReferenceLines}}<span class="reference-line" data-kind="{{$line.Kind}}"><i aria-hidden="true"></i>{{$line.Label}} <b>{{metricValue $metric $line.Value}}</b></span>{{else}}<span class="no-reference">No reference lines</span>{{end}}
                  </div>{{end}}
                </div>{{else}}—{{end}}
              </td>
              <td class="numeric">{{.Spec.Priority}}</td>
              <td><time datetime="{{isoTime .CreatedAt}}">{{jobTime .CreatedAt}}</time></td>
            </tr>
            {{else}}
            <tr><td class="empty" colspan="7">No jobs submitted.</td></tr>
            {{end}}
          </tbody>
        </table>
      </div>
    </section>

    <section class="panel" aria-labelledby="workers-title">
      <div class="panel-header"><h2 id="workers-title">Workers</h2><span>{{len .Workers}} enrolled</span></div>
      {{if .Workers}}<div class="table-scroll"><table>
        <thead><tr><th>Status</th><th>Worker</th><th>Capacity</th><th>Labels</th><th>Last seen</th><th>Agent</th></tr></thead>
        <tbody>{{range .Workers}}<tr>
          <td><span class="state" data-state="{{.Status}}">{{.Status}}</span></td>
          <td><strong>{{.Name}}</strong><small>{{.ID}}</small></td>
          <td>{{workerResources .Capacity}}</td>
          <td><small class="wide">{{labels .Capacity.Labels}}</small></td>
          <td>{{optionalTime .LastSeenAt}}</td>
          <td><small class="wide" title="{{.AgentVersion}}">{{.AgentVersion}}</small></td>
        </tr>{{end}}</tbody>
      </table></div>{{else}}
      <div class="empty-block"><strong>No workers enrolled.</strong><span>Provision a worker-bound credential with <code>lcjs worker create</code>.</span></div>
      {{end}}
    </section>
  </main>
</body>
</html>`))

var jobTemplate = template.Must(template.New("job").Funcs(pageFunctions).Parse(`<!doctype html>
<html lang="en" data-theme="light">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta http-equiv="refresh" content="15">
  <title>{{.Detail.Job.Spec.Name}} · Compute Jobs</title>
  <link rel="stylesheet" href="/static/app.css?v=3">
  <script src="/static/theme.js"></script>
</head>
<body>
  <header class="topbar">
    <a class="brand" href="/">Compute Jobs</a>
    <nav class="toolbar" aria-label="Console controls">
      <a href="/">Overview</a>
      <label class="theme-control">Theme
        <select data-theme-picker aria-label="Color theme">
          <option value="light">Light</option>
          <option value="dark">Dark</option>
          <option value="system">System</option>
        </select>
      </label>
      <form method="post" action="/logout"><button class="button-secondary" type="submit">Sign out</button></form>
    </nav>
  </header>
  <main class="console">
    <div class="section-heading">
      <div><h1>{{.Detail.Job.Spec.Name}} <span class="state" data-state="{{.Detail.Job.State}}">{{.Detail.Job.State}}</span></h1><p>{{.Detail.Job.Spec.Project}} · {{.Detail.Job.ID}} · refreshes every 15 seconds</p></div>
      <span class="identity">{{.TokenName}}</span>
    </div>

    <section class="panel" aria-labelledby="spec-title">
      <div class="panel-header"><h2 id="spec-title">Execution</h2><span>revision {{.Detail.Job.Revision}}</span></div>
      <dl class="detail-grid">
        <div><dt>Command</dt><dd><code>{{command .Detail.Job.Spec.Command}}</code></dd></div>
        <div><dt>Source</dt><dd><code>{{.Detail.Job.Spec.Source.GitURL}}</code><small>{{.Detail.Job.Spec.Source.Commit}}</small></dd></div>
        <div><dt>Resources</dt><dd>{{resources .Detail.Job.Spec.Resources}}</dd></div>
        <div><dt>Created</dt><dd>{{jobTime .Detail.Job.CreatedAt}}</dd></div>
      </dl>
    </section>

    <section class="panel" aria-labelledby="attempts-title">
      <div class="panel-header"><h2 id="attempts-title">Attempts</h2><span>{{len .Detail.Attempts}} total</span></div>
      <div class="table-scroll"><table>
        <thead><tr><th>#</th><th>State</th><th>Worker</th><th>Started</th><th>Finished</th><th>Exit</th><th>Log</th><th>Error</th></tr></thead>
        <tbody>{{range .Detail.Attempts}}<tr>
          <td class="numeric">{{.AttemptNumber}}</td>
          <td><span class="state" data-state="{{.State}}">{{.State}}</span><small>{{.ID}}</small></td>
          <td><small class="wide">{{.WorkerID}}</small></td>
          <td>{{optionalTime .StartedAt}}</td><td>{{optionalTime .FinishedAt}}</td><td class="numeric">{{exitCode .ExitCode}}</td>
          <td><small class="wide" title="{{.LogURI}}">{{if .LogURI}}{{.LogURI}}{{else}}—{{end}}</small></td>
          <td class="error-cell">{{if .Error}}{{.Error}}{{else}}—{{end}}</td>
        </tr>{{else}}<tr><td class="empty" colspan="8">No attempt has been assigned.</td></tr>{{end}}</tbody>
      </table></div>
      {{range .Detail.Attempts}}{{if .LogTail}}<details class="log-tail" open><summary>Attempt {{.AttemptNumber}} recent log (last 64 KiB)</summary><pre>{{.LogTail}}</pre></details>{{end}}{{end}}
    </section>

    <section class="panel" aria-labelledby="metrics-title">
      <div class="panel-header"><h2 id="metrics-title">Metrics</h2><span>{{len .Detail.Metrics}} samples</span></div>
      {{if .Metrics}}<div class="metric-grid">{{range $series := .Metrics}}<article class="metric-chart">
        <header><div><strong>{{metricName .Definition}}</strong><small>{{metricObjective .Definition.Objective}}</small></div><b>{{with .Latest}}{{metricValue $series.Definition .Value}}{{else}}No samples{{end}}</b></header>
        <svg viewBox="0 0 100 40" preserveAspectRatio="none" role="img" aria-label="{{metricName .Definition}} samples and reference lines">
          <line class="chart-axis" x1="4" y1="36" x2="96" y2="36"></line>
          {{range .References}}<line class="chart-reference" data-kind="{{.Line.Kind}}" x1="4" y1="{{.Y}}" x2="96" y2="{{.Y}}"></line>{{end}}
          {{if .HasSamples}}<polyline class="chart-series" points="{{.Points}}"></polyline><circle class="chart-point" cx="{{.LatestX}}" cy="{{.LatestY}}" r="1.8"></circle>{{end}}
        </svg>
        <div class="metric-references">{{range .References}}<span class="reference-line" data-kind="{{.Line.Kind}}"><i aria-hidden="true"></i>{{.Line.Label}} <b>{{metricValue $series.Definition .Line.Value}}</b></span>{{else}}<span class="no-reference">No reference lines</span>{{end}}</div>
      </article>{{end}}</div>{{else}}<div class="empty-block"><strong>No metrics declared.</strong></div>{{end}}
    </section>

    <section class="panel" aria-labelledby="artifacts-title">
      <div class="panel-header"><h2 id="artifacts-title">Artifacts</h2><span>{{len .Detail.Artifacts}} recorded</span></div>
      <div class="table-scroll"><table>
        <thead><tr><th>Name</th><th>Size</th><th>SHA-256</th><th>Location</th><th>Created</th></tr></thead>
        <tbody>{{range .Detail.Artifacts}}<tr><td><strong>{{.Name}}</strong></td><td class="numeric">{{.SizeBytes}} B</td><td><small class="wide">{{.SHA256}}</small></td><td><small class="wide">{{.URI}}</small></td><td>{{jobTime .CreatedAt}}</td></tr>{{else}}<tr><td class="empty" colspan="5">No artifacts recorded.</td></tr>{{end}}</tbody>
      </table></div>
    </section>
  </main>
</body>
</html>`))

func renderPage(response http.ResponseWriter, page *template.Template, data any) {
	renderPageStatus(response, page, data, http.StatusOK)
}

func renderPageStatus(response http.ResponseWriter, page *template.Template, data any, status int) {
	setPageHeaders(response)
	response.WriteHeader(status)
	_ = page.Execute(response, data)
}

func setPageHeaders(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
}

func serveStyles(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/css; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(response, appCSS)
}

func serveThemeScript(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(response, themeJS)
}

const themeJS = `(function () {
  var storageKey = "lcjs-theme";
  var media = window.matchMedia("(prefers-color-scheme: dark)");
  function preference() {
    var saved = localStorage.getItem(storageKey);
    return saved === "dark" || saved === "system" ? saved : "light";
  }
  function apply(value) {
    document.documentElement.dataset.theme = value === "system" ? (media.matches ? "dark" : "light") : value;
    document.querySelectorAll("[data-theme-picker]").forEach(function (picker) { picker.value = value; });
  }
  apply(preference());
  window.addEventListener("DOMContentLoaded", function () {
    apply(preference());
    document.querySelectorAll("[data-theme-picker]").forEach(function (picker) {
      picker.addEventListener("change", function () { localStorage.setItem(storageKey, picker.value); apply(picker.value); });
    });
  });
  media.addEventListener("change", function () { if (preference() === "system") apply("system"); });
}());`

const appCSS = `
:root {
  color-scheme: light;
  --bg: #f5f6f8; --panel: #fff; --panel-alt: #f8f9fb; --text: #1c2028;
  --muted: #677080; --border: #d7dbe2; --border-strong: #b8bec8; --accent: #2256a3;
  --success: #19713b; --success-bg: #eaf6ee; --danger: #b42318; --danger-bg: #fef0ef;
}
html[data-theme="dark"] {
  color-scheme: dark;
  --bg: #15171b; --panel: #1d2025; --panel-alt: #191c20; --text: #e8eaed;
  --muted: #9da5b1; --border: #353a43; --border-strong: #505763; --accent: #8ab4f8;
  --success: #78d99a; --success-bg: #153824; --danger: #ff9b93; --danger-bg: #461d1b;
}
* { box-sizing: border-box; }
html { min-width: 320px; background: var(--bg); }
body { margin: 0; background: var(--bg); color: var(--text); font: 13px/1.4 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
button, input, select { color: inherit; font: inherit; }
a { color: var(--accent); }
.topbar { min-height: 45px; display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 7px 16px; background: var(--panel); border-bottom: 1px solid var(--border); }
.brand { color: var(--text); font-size: 15px; font-weight: 700; text-decoration: none; letter-spacing: -.01em; }
.toolbar { display: flex; align-items: center; gap: 14px; }
.toolbar form { margin: 0; }
.online { display: inline-flex; align-items: center; gap: 6px; color: var(--success); font-size: 12px; font-weight: 650; }
.online span { width: 7px; height: 7px; border-radius: 50%; background: var(--success); }
.theme-control { display: inline-flex; align-items: center; gap: 6px; color: var(--muted); font-size: 12px; }
select, input { border: 1px solid var(--border-strong); border-radius: 4px; background: var(--panel); }
select { padding: 4px 24px 4px 7px; }
button { min-height: 29px; padding: 4px 11px; border: 1px solid #184784; border-radius: 4px; background: #2256a3; color: #fff; cursor: pointer; font-weight: 650; }
button:hover { background: #1d4b8f; }
.button-secondary { border-color: var(--border-strong); background: var(--panel); color: var(--text); font-weight: 550; }
.button-secondary:hover { background: var(--panel-alt); }
.console { padding: 14px 16px 28px; }
.section-heading { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; margin-bottom: 10px; }
h1, h2, p { margin: 0; }
h1 { font-size: 17px; line-height: 1.25; }
.section-heading p { margin-top: 2px; color: var(--muted); font-size: 12px; }
.identity { color: var(--muted); font: 12px ui-monospace, SFMono-Regular, Consolas, monospace; }
.stats { display: grid; grid-template-columns: repeat(5, minmax(100px, 1fr)); margin: 0 0 12px; border: 1px solid var(--border); background: var(--panel); }
.stats div { min-height: 58px; padding: 9px 12px; border-right: 1px solid var(--border); }
.stats div:last-child { border-right: 0; }
.stats dt { color: var(--muted); font-size: 11px; font-weight: 650; letter-spacing: .04em; text-transform: uppercase; }
.stats dd { margin: 2px 0 0; font-size: 20px; font-variant-numeric: tabular-nums; font-weight: 650; }
.panel { margin-top: 12px; border: 1px solid var(--border); background: var(--panel); }
.panel-header { min-height: 38px; display: flex; align-items: center; justify-content: space-between; padding: 7px 10px; border-bottom: 1px solid var(--border); }
.panel-header h2 { font-size: 13px; }
.panel-header span { color: var(--muted); font-size: 11px; }
.table-scroll { overflow-x: auto; }
table { width: 100%; border-collapse: collapse; text-align: left; white-space: nowrap; }
th { padding: 6px 9px; background: var(--panel-alt); border-bottom: 1px solid var(--border); color: var(--muted); font-size: 10px; letter-spacing: .045em; text-transform: uppercase; }
td { padding: 7px 9px; border-bottom: 1px solid var(--border); vertical-align: top; }
tbody tr:last-child td { border-bottom: 0; }
tbody tr:hover td { background: var(--panel-alt); }
td strong { display: block; font-weight: 650; }
td strong a { color: var(--text); }
td small { display: block; max-width: 360px; overflow: hidden; color: var(--muted); font: 10px/1.35 ui-monospace, SFMono-Regular, Consolas, monospace; text-overflow: ellipsis; }
td small.command { max-width: 440px; margin-top: 2px; }
td small.wide { max-width: 520px; }
.metric-list { display: grid; gap: 5px; min-width: 190px; white-space: normal; }
.metric-definition { display: grid; gap: 2px; }
.metric-definition > div { display: flex; align-items: baseline; gap: 6px; }
.metric-definition strong { display: inline; font-size: 11px; }
.metric-definition .objective { display: inline; color: var(--muted); font: 9px ui-sans-serif, system-ui, sans-serif; text-transform: uppercase; }
.reference-line { display: inline-flex; align-items: center; gap: 5px; color: var(--muted); font-size: 10px; }
.reference-line i { width: 16px; height: 0; border-top: 1px dashed var(--accent); }
.reference-line[data-kind="goal"] i { border-top-style: solid; border-top-width: 2px; }
.reference-line[data-kind="baseline"] i { border-top-color: var(--border-strong); }
.reference-line[data-kind="threshold"] i { border-top-color: var(--danger); }
.reference-line b { color: var(--text); font-variant-numeric: tabular-nums; }
.no-reference { color: var(--muted); font-size: 10px; }
.numeric { text-align: right; font-variant-numeric: tabular-nums; }
.state { display: inline-block; padding: 1px 6px; border: 1px solid var(--border-strong); border-radius: 10px; font-size: 10px; font-weight: 700; }
.state[data-state="running"], .state[data-state="succeeded"], .state[data-state="online"] { border-color: var(--success); background: var(--success-bg); color: var(--success); }
.state[data-state="failed"], .state[data-state="canceled"], .state[data-state="offline"] { border-color: var(--danger); background: var(--danger-bg); color: var(--danger); }
.empty { height: 72px; color: var(--muted); text-align: center; vertical-align: middle; }
.empty-block { display: flex; flex-direction: column; align-items: center; justify-content: center; min-height: 76px; padding: 14px; color: var(--muted); }
.empty-block strong { color: var(--text); }
.detail-grid { display: grid; grid-template-columns: minmax(240px, 2fr) minmax(300px, 3fr) minmax(120px, 1fr) minmax(160px, 1fr); margin: 0; }
.detail-grid > div { min-width: 0; padding: 9px 10px; border-right: 1px solid var(--border); }
.detail-grid > div:last-child { border-right: 0; }
.detail-grid dt { margin-bottom: 3px; color: var(--muted); font-size: 10px; font-weight: 650; letter-spacing: .04em; text-transform: uppercase; }
.detail-grid dd { min-width: 0; margin: 0; overflow: hidden; text-overflow: ellipsis; }
.detail-grid code, .empty-block code { font: 11px ui-monospace, SFMono-Regular, Consolas, monospace; }
.detail-grid small { display: block; overflow: hidden; color: var(--muted); font: 10px ui-monospace, SFMono-Regular, Consolas, monospace; text-overflow: ellipsis; }
.error-cell { max-width: 360px; white-space: normal; color: var(--danger); }
.log-tail { border-top: 1px solid var(--border); }
.log-tail summary { padding: 7px 10px; cursor: pointer; color: var(--muted); font-size: 11px; font-weight: 650; }
.log-tail pre { max-height: 300px; margin: 0; overflow: auto; padding: 10px; background: var(--panel-alt); border-top: 1px solid var(--border); font: 11px/1.45 ui-monospace, SFMono-Regular, Consolas, monospace; white-space: pre-wrap; }
.metric-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); }
.metric-chart { min-width: 0; padding: 10px; border-right: 1px solid var(--border); border-bottom: 1px solid var(--border); }
.metric-chart header { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; }
.metric-chart header strong { display: block; }
.metric-chart header small { margin-left: 6px; color: var(--muted); font-size: 9px; text-transform: uppercase; }
.metric-chart header b { font-size: 16px; font-variant-numeric: tabular-nums; }
.metric-chart svg { display: block; width: 100%; height: 150px; margin-top: 5px; background: var(--panel-alt); border: 1px solid var(--border); }
.chart-axis { stroke: var(--border-strong); stroke-width: .4; }
.chart-reference { stroke: var(--accent); stroke-width: .5; stroke-dasharray: 2 1; }
.chart-reference[data-kind="goal"] { stroke-width: .9; stroke-dasharray: none; }
.chart-reference[data-kind="threshold"] { stroke: var(--danger); }
.chart-series { fill: none; stroke: var(--success); stroke-width: 1.1; vector-effect: non-scaling-stroke; }
.chart-point { fill: var(--success); vector-effect: non-scaling-stroke; }
.metric-references { display: flex; flex-wrap: wrap; gap: 4px 14px; margin-top: 5px; }
.login-main { width: min(480px, calc(100% - 32px)); margin: 44px auto; }
.login-panel { border: 1px solid var(--border); background: var(--panel); padding: 18px; }
.login-panel h1 { margin-bottom: 5px; }
.login-panel > p { color: var(--muted); }
.login-panel form { display: grid; gap: 6px; margin-top: 18px; }
.login-panel form label { font-weight: 650; }
.login-panel input { width: 100%; padding: 8px 9px; font: 13px ui-monospace, SFMono-Regular, Consolas, monospace; }
.login-panel form button { justify-self: start; margin-top: 4px; }
.login-panel .fine { margin-top: 15px; font-size: 11px; }
.notice { margin-top: 12px; padding: 7px 9px; border: 1px solid var(--border); }
.notice.error { border-color: var(--danger); background: var(--danger-bg); color: var(--danger); }
@media (max-width: 700px) {
  .topbar { align-items: flex-start; padding: 9px 12px; }
  .toolbar { flex-wrap: wrap; justify-content: flex-end; gap: 7px 10px; }
  .online { width: 100%; justify-content: flex-end; }
  .console { padding: 12px; }
  .stats { grid-template-columns: repeat(2, 1fr); }
  .stats div { border-bottom: 1px solid var(--border); }
  .stats div:nth-child(2n) { border-right: 0; }
  .stats div:last-child { border-bottom: 0; }
  .section-heading { align-items: flex-start; flex-direction: column; }
  .detail-grid { grid-template-columns: 1fr; }
  .detail-grid > div { border-right: 0; border-bottom: 1px solid var(--border); }
  .detail-grid > div:last-child { border-bottom: 0; }
}
`
