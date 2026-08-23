# Metrics, objectives, and reference lines

A job may declare the scalar series that matter before it starts. Each metric
definition answers three separate questions:

1. **Presentation:** Is the raw value a number or a 0-to-1 fraction displayed
   as a percentage?
2. **Objective:** Is higher better, lower better, or is the series descriptive?
3. **Reference lines:** Which named values should be drawn across the chart?

A reference line is also commonly called a benchmark, target, threshold, or
baseline. LCJS uses `reference_lines` as the general mechanism and a `kind` to
preserve that meaning.

## Example

This definition renders `0.947` as `94.7%`, treats larger values as better, and
marks three horizontal comparisons:

```json
{
  "metrics": [{
    "name": "validation/accuracy",
    "display_name": "Validation accuracy",
    "format": "percent",
    "precision": 1,
    "objective": "maximize",
    "reference_lines": [
      {"kind": "baseline", "label": "Current model", "value": 0.912},
      {"kind": "benchmark", "label": "Human best", "value": 0.945},
      {"kind": "goal", "label": "Project goal", "value": 0.960}
    ]
  }]
}
```

Put `metrics` at the top level of the immutable job specification submitted to
`POST /api/v1/jobs`. The dashboard immediately lists the configured metrics
and references for that job.

With the complete job specification saved as `job.json`, submit it with:

```sh
curl https://control.example.com/api/v1/jobs \
  --request POST \
  --header "Authorization: Bearer $LCJS_TOKEN" \
  --header "Content-Type: application/json" \
  --header "Idempotency-Key: experiment-2026-01-01-a" \
  --data-binary @job.json
```

Use a new idempotency key when the specification changes. Reusing the same key
with the same document safely returns the original job; reusing it with a
different document is rejected.

## Fields

| Field | Meaning |
| --- | --- |
| `name` | Stable series key emitted by the job, such as `validation/accuracy` |
| `display_name` | Optional human-facing label; defaults to `name` |
| `format` | `number` (default) or `percent` |
| `unit` | Optional suffix for a number, such as `ms` or `tokens/s` |
| `precision` | Digits after the decimal point, from 0 through 9 |
| `objective` | `maximize`, `minimize`, or `none` (default) |
| `reference_lines` | Up to 16 named horizontal chart annotations |

Reference-line kinds are:

- `goal`: a value this run or project is intended to reach
- `benchmark`: an external comparison, such as published or human performance
- `baseline`: an existing model or control result
- `threshold`: a meaningful boundary, such as minimum acceptable accuracy

All reference values use the same raw scale as samples. In particular,
`format: "percent"` requires fractional values from `0` through `1`; use
`0.945`, not `94.5`.

Reference lines are annotations, not automation. `objective` tells comparison
and chart code which direction is better, but it does not stop a job. Use a
job `health` policy for alerts, cancellation, retry, or checkpoint-and-stop
behavior.

## Reporting samples

Metric samples use the definition's exact `name`, a finite scalar `value`, and
either a monotonically increasing `step` or an observation time. The worker
telemetry payload will use this shape:

```json
{
  "name": "validation/accuracy",
  "value": 0.947,
  "step": 1200,
  "observed_at": "2026-01-01T00:20:00Z"
}
```

The current control-plane slice stores and displays metric definitions and
their reference lines. Sample ingestion, roll-ups, and time-series charts are
part of the durable worker telemetry slice; job code should keep emitting the
stable name and raw scale above so those samples can be connected without
changing the job contract.

When chart samples are available, the renderer must include both observed and
reference values in the y-axis domain, label each line, preserve its `kind`,
and show the latest/best value relative to the declared objective.
