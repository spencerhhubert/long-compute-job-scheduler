// Live metric charts for the job detail page. Renders one uPlot panel per
// metric series, polls /jobs/{id}/metrics.json with the incremental cursor
// every 5 seconds while the job is active, and stops on a terminal state.
(function () {
  "use strict";
  var POLL_MS = 5000;
  var TERMINAL = ["succeeded", "failed", "canceled"];
  var container = document.querySelector("[data-metric-charts]");
  if (!container || typeof uPlot === "undefined") return;
  var jobId = container.dataset.jobId;
  var metricsMeta = document.querySelector("[data-metrics-meta]");
  var charts = new Map();
  var cursor = 0;
  var totalPoints = 0;
  var knownState = null;

  function cssColor(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function formatValue(def, value, forTick) {
    var unit = def.unit || "";
    var precision = 3;
    if (def.format === "percent") {
      precision = 1;
      value *= 100;
      unit = "%";
    } else if (def.format === "bytes") {
      precision = 1;
      var units = ["B", "KiB", "MiB", "GiB", "TiB"];
      var index = 0;
      while (value >= 1024 && index < units.length - 1) {
        value /= 1024;
        index += 1;
      }
      unit = units[index];
      if (unit === "B") precision = 0;
    }
    if (def.precision != null) precision = def.precision;
    var text = value.toFixed(precision);
    if (forTick) text = String(Number(text));
    if (unit === "%") return text + "%";
    if (unit && (def.format === "bytes" || !forTick)) return text + " " + unit;
    return text;
  }

  // The plotted line is the latest attempt: a retry restarts the series.
  function activePoints(points) {
    var latest = 0;
    points.forEach(function (point) { latest = Math.max(latest, point.attempt); });
    return points.filter(function (point) { return point.attempt === latest; });
  }

  function buildData(chart) {
    var points = activePoints(chart.points);
    var hasSteps = points.some(function (point) { return point.step != null; });
    if (hasSteps) points = points.filter(function (point) { return point.step != null; });
    var xOf = hasSteps
      ? function (point) { return point.step; }
      : function (point) { return Date.parse(point.observed_at) / 1000; };
    points.sort(function (a, b) { return xOf(a) - xOf(b); });
    return { xIsTime: !hasSteps, x: points.map(xOf), y: points.map(function (point) { return point.value; }) };
  }

  function yRange(chart) {
    var min = Infinity;
    var max = -Infinity;
    chart.data.y.forEach(function (value) {
      min = Math.min(min, value);
      max = Math.max(max, value);
    });
    (chart.def.reference_lines || []).forEach(function (line) {
      min = Math.min(min, line.value);
      max = Math.max(max, line.value);
    });
    if (!isFinite(min)) { min = 0; max = 1; }
    if (min === max) {
      var flat = Math.abs(min) * 0.1 || 1;
      min -= flat;
      max += flat;
    } else {
      var pad = (max - min) * 0.08;
      min -= pad;
      max += pad;
    }
    if (chart.def.format === "percent") {
      min = Math.max(min, -0.001);
      max = Math.min(max, 1.001);
    }
    return [min, max];
  }

  var referenceStyle = {
    goal: { color: "--chart-series", dash: [6, 4] },
    benchmark: { color: "--muted", dash: [6, 4] },
    baseline: { color: "--muted", dash: [2, 3] },
    threshold: { color: "--danger", dash: [6, 4] }
  };

  function drawReferenceLines(u, chart) {
    var refs = (chart.def.reference_lines || []).slice();
    if (!refs.length) return;
    var ratio = uPlot.pxRatio;
    var ctx = u.ctx;
    var labelInk = cssColor("--muted");
    var labelBack = cssColor("--panel-alt");
    var used = [];
    refs.sort(function (a, b) { return b.value - a.value; });
    refs.forEach(function (line) {
      var style = referenceStyle[line.kind] || referenceStyle.benchmark;
      var y = Math.round(u.valToPos(line.value, "y", true));
      ctx.save();
      ctx.beginPath();
      ctx.setLineDash(style.dash.map(function (px) { return px * ratio; }));
      ctx.lineWidth = ratio;
      ctx.strokeStyle = cssColor(style.color);
      ctx.moveTo(u.bbox.left, y);
      ctx.lineTo(u.bbox.left + u.bbox.width, y);
      ctx.stroke();
      // Right-aligned label in the console's muted ink, nudged below the
      // line when the slot above it is off-canvas or already occupied.
      var text = line.label + " " + formatValue(chart.def, line.value);
      var height = 11 * ratio;
      var top = y - 3 * ratio - height;
      if (top < u.bbox.top || used.some(function (slot) { return top < slot[1] && top + height > slot[0]; })) {
        top = y + 3 * ratio;
      }
      used.push([top, top + height]);
      ctx.setLineDash([]);
      ctx.font = 10 * ratio + "px ui-sans-serif, system-ui, sans-serif";
      ctx.textAlign = "right";
      ctx.textBaseline = "top";
      var right = u.bbox.left + u.bbox.width - 4 * ratio;
      var width = ctx.measureText(text).width;
      ctx.fillStyle = labelBack;
      ctx.fillRect(right - width - 3 * ratio, top - ratio, width + 6 * ratio, height + 2 * ratio);
      ctx.fillStyle = labelInk;
      ctx.fillText(text, right, top);
      ctx.restore();
    });
  }

  function attachTooltip(u, chart) {
    var tip = document.createElement("div");
    tip.className = "chart-tooltip";
    tip.style.display = "none";
    var value = document.createElement("b");
    var label = document.createElement("span");
    tip.appendChild(value);
    tip.appendChild(label);
    u.over.appendChild(tip);
    chart.tooltip = function () {
      var index = u.cursor.idx;
      if (index == null || u.data[1][index] == null) {
        tip.style.display = "none";
        return;
      }
      value.textContent = formatValue(chart.def, u.data[1][index]);
      label.textContent = chart.data.xIsTime
        ? new Date(u.data[0][index] * 1000).toLocaleString()
        : "step " + u.data[0][index];
      tip.style.display = "block";
      var left = u.cursor.left;
      var flip = left > u.over.clientWidth - tip.offsetWidth - 16;
      tip.style.left = (flip ? left - tip.offsetWidth - 10 : left + 10) + "px";
      tip.style.top = Math.min(Math.max(u.cursor.top - 10, 0), u.over.clientHeight - tip.offsetHeight) + "px";
    };
  }

  function plotSize(chart) {
    return { width: Math.max(chart.plotEl.clientWidth - 8, 160), height: 190 };
  }

  // Size the y axis to its widest formatted tick so labels never clip.
  function measuredAxisSize(u, values, axisIndex, cycle) {
    if (cycle > 1) return u.axes[axisIndex]._size;
    if (!values) return 60;
    var ratio = uPlot.pxRatio;
    u.ctx.save();
    u.ctx.font = 11 * ratio + "px ui-sans-serif, system-ui, sans-serif";
    var widest = 0;
    values.forEach(function (value) {
      widest = Math.max(widest, u.ctx.measureText(value).width);
    });
    u.ctx.restore();
    return Math.ceil(widest / ratio) + 18;
  }

  function makePlot(chart) {
    if (chart.plot) chart.plot.destroy();
    chart.data = buildData(chart);
    var series = cssColor("--chart-series");
    var grid = { stroke: cssColor("--border"), width: 1 };
    var axis = {
      stroke: cssColor("--muted"),
      font: "11px ui-sans-serif, system-ui, sans-serif",
      grid: grid,
      ticks: { stroke: cssColor("--border"), width: 1, size: 4 }
    };
    var options = {
      width: plotSize(chart).width,
      height: plotSize(chart).height,
      legend: { show: false },
      cursor: {
        y: false,
        points: { size: 8, fill: series, stroke: series }
      },
      scales: {
        x: { time: chart.data.xIsTime },
        y: { range: function () { return yRange(chart); } }
      },
      series: [
        {},
        { stroke: series, width: 2, points: { show: false } }
      ],
      axes: [
        Object.assign({}, axis, { space: chart.data.xIsTime ? 90 : 50 }),
        Object.assign({}, axis, {
          size: measuredAxisSize,
          values: function (u, splits) {
            return splits.map(function (split) { return formatValue(chart.def, split, true); });
          }
        })
      ],
      hooks: {
        // Reference lines render between the grid and the series so the
        // data always stays on top of its annotations.
        drawAxes: [function (u) { drawReferenceLines(u, chart); }],
        setCursor: [function () { if (chart.tooltip) chart.tooltip(); }]
      }
    };
    chart.plot = new uPlot(options, [chart.data.x, chart.data.y], chart.plotEl);
    attachTooltip(chart.plot, chart);
  }

  function objectiveText(objective) {
    if (objective === "maximize") return "↑ maximize";
    if (objective === "minimize") return "↓ minimize";
    return "";
  }

  function ensureChart(seriesPayload) {
    var def = seriesPayload.definition;
    var chart = charts.get(def.name);
    if (chart) return chart;
    var panel = document.createElement("article");
    panel.className = "metric-chart";
    var header = document.createElement("header");
    var titles = document.createElement("div");
    var title = document.createElement("strong");
    title.textContent = def.display_name || def.name;
    titles.appendChild(title);
    var objective = objectiveText(def.objective);
    if (objective) {
      var small = document.createElement("small");
      small.textContent = objective;
      titles.appendChild(small);
    }
    var latest = document.createElement("b");
    latest.textContent = "No samples";
    header.appendChild(titles);
    header.appendChild(latest);
    panel.appendChild(header);
    var plotEl = document.createElement("div");
    plotEl.className = "chart-plot";
    panel.appendChild(plotEl);
    container.appendChild(panel);
    chart = { def: def, points: [], plotEl: plotEl, latestEl: latest, plot: null };
    charts.set(def.name, chart);
    makePlot(chart);
    return chart;
  }

  function apply(payload) {
    var panelCount = charts.size;
    payload.metrics.forEach(function (seriesPayload) {
      var chart = ensureChart(seriesPayload);
      if (!seriesPayload.points.length) return;
      chart.points = chart.points.concat(seriesPayload.points);
      totalPoints += seriesPayload.points.length;
      makePlot(chart);
      if (chart.data.y.length) {
        chart.latestEl.textContent = formatValue(chart.def, chart.data.y[chart.data.y.length - 1]);
      }
    });
    // Adding a panel re-flows the auto-fit grid, so every plot re-measures.
    if (charts.size !== panelCount) resizeAll();
    if (metricsMeta) metricsMeta.textContent = totalPoints + " samples";
    if (!charts.size && !container.querySelector(".empty-block")) {
      var empty = document.createElement("div");
      empty.className = "empty-block";
      var strong = document.createElement("strong");
      strong.textContent = "No metrics declared.";
      empty.appendChild(strong);
      container.appendChild(empty);
    }
  }

  function poll() {
    var url = "/jobs/" + encodeURIComponent(jobId) + "/metrics.json" + (cursor ? "?after=" + cursor : "");
    return fetch(url).then(function (response) {
      if (response.status === 401) {
        window.location.reload();
        throw new Error("session expired");
      }
      if (!response.ok) throw new Error("metrics request failed: " + response.status);
      return response.json();
    }).then(function (payload) {
      cursor = payload.cursor;
      apply(payload);
      // A state change redraws the whole page so attempts, logs, and state
      // chips stay truthful; polling stops on its own at a terminal state.
      if (knownState !== null && knownState !== payload.job_state) {
        window.location.reload();
        return payload.job_state;
      }
      knownState = payload.job_state;
      return payload.job_state;
    });
  }

  function schedule() {
    setTimeout(function () {
      poll().catch(function () { return knownState; }).then(function (state) {
        if (TERMINAL.indexOf(state) === -1) schedule();
      });
    }, POLL_MS);
  }

  // setSize resets uPlot's cursor, so only genuinely changed sizes apply.
  function resizeAll() {
    charts.forEach(function (chart) {
      if (!chart.plot) return;
      var size = plotSize(chart);
      if (size.width !== chart.plot.width || size.height !== chart.plot.height) {
        chart.plot.setSize(size);
      }
    });
  }

  var resizeTimer = null;
  window.addEventListener("resize", function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(resizeAll, 120);
  });
  new MutationObserver(function () {
    charts.forEach(function (chart) { makePlot(chart); });
  }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

  poll().then(function (state) {
    if (TERMINAL.indexOf(state) === -1) schedule();
  }).catch(function () {
    schedule();
  });
}());
