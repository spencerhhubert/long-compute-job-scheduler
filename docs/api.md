# API draft

Status: design draft; incompatible changes are expected before the first tag.

The control plane exposes `/api/v1`. JSON uses RFC 3339 timestamps in UTC,
opaque string IDs, byte counts as integers, and durations as strings such as
`"30s"` or `"2h"`.

## Conventions

- `Authorization: Bearer <token>` authenticates every JSON endpoint except
  `/healthz`. The browser console exchanges an operator token at `/login` for a
  persistent opaque session cookie; the raw token is not stored in the browser.
- Mutating client requests accept an `Idempotency-Key` header.
- List endpoints use opaque cursor pagination.
- Errors use a stable machine-readable code:

```json
{
  "error": {
    "code": "revision_conflict",
    "message": "job changed since revision 4",
    "request_id": "req_..."
  }
}
```

The protocol never includes host absolute paths or secret values.

## Operator API

Initial resources and operations:

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v1/jobs` | Submit an immutable job spec |
| `GET` | `/api/v1/jobs` | List jobs from the central projection |
| `GET` | `/api/v1/jobs/{id}` | Read job, attempts, and recent events |
| `GET` | `/api/v1/jobs/{id}/metrics` | Read metric series with an incremental cursor |
| `POST` | `/api/v1/jobs/{id}/cancel` | Set desired state to canceled |
| `POST` | `/api/v1/jobs/{id}/retry` | Create a retry after policy validation |
| `GET` | `/api/v1/workers` | List last-known worker state and resources |
| `GET` | `/api/v1/events` | Cursor through the audit/event stream |
| `POST` | `/api/v1/tokens` | Mint a scoped token; raw value returned once |
| `DELETE` | `/api/v1/tokens/{id}` | Revoke a token |

Example job submission:

```json
{
  "project": "example-research",
  "name": "train-baseline",
  "priority": 0,
  "command": ["python", "train.py", "--config", "baseline.toml"],
  "resources": {
    "cpu": 4,
    "memory_bytes": 17179869184,
    "gpus": [{"count": 1, "memory_bytes": 12884901888, "sharing": "exclusive"}]
  },
  "constraints": {"labels": {"cuda": "true"}},
  "retry": {"max_attempts": 2, "lost_worker": "manual"},
  "checkpoint": {
    "glob": "checkpoints/*.pt",
    "resume_command": ["python", "train.py", "--resume", "${LCJS_CHECKPOINT}"]
  },
  "artifacts": [{"name": "checkpoints", "glob": "checkpoints/*.pt"}],
  "metrics": [{
    "name": "validation/accuracy",
    "display_name": "Validation accuracy",
    "format": "percent",
    "precision": 1,
    "objective": "maximize",
    "reference_lines": [
      {"kind": "benchmark", "label": "Human best", "value": 0.945},
      {"kind": "goal", "label": "Project goal", "value": 0.960}
    ]
  }],
  "health": [{
    "kind": "metric_stalled",
    "metric": "validation/loss",
    "mode": "min",
    "window": "1h",
    "minimum_delta": 0.001,
    "action": "notify",
    "target": "research-session"
  }]
}
```

The stored spec is immutable. Cancel, retry, priority changes, and policy
overrides are separate commands with audit events.

A job runs in the directory the assigned worker maps to its `project`; only
workers advertising that project are offered the job. An optional
`source` object of the form `{"commit": "<full object ID>", "git_url": "..."}`
pins the revision; without it the job runs at the directory's current state.
Each attempt in the job detail response carries a `git` object with the
`commit`, `branch`, and `dirty` flag the worker observed at attempt start, or
no `git` object when the directory is not a git repository.

## Worker enrollment

An operator mints a one-use, expiring enrollment token constrained by optional
labels. The new worker generates a local identity, then calls:

```text
POST /api/v1/enroll
```

The response contains the stable worker ID and a worker-bound token, shown only
in that response. The worker persists it in a permission-restricted local config
file. Re-enrollment creates a new identity; credential rotation is a distinct
authenticated operation.

## Worker sync

```text
POST /api/v1/worker/sync
```

One request carries a resource snapshot (including the worker's advertised
project names, which constrain which jobs it can be offered), lease renewals,
and a bounded ordered batch from the local outbox:

```json
{
  "worker_id": "wrk_...",
  "session_id": "ses_...",
  "agent_version": "0.1.0",
  "resources": {
    "observed_at": "2026-01-01T00:00:00Z",
    "cpu_total": 16,
    "memory_bytes_total": 68719476736,
    "gpus": []
  },
  "leases": [{"attempt_id": "att_...", "revision": 3}],
  "events": [{
    "sequence": 41,
    "event_id": "evt_...",
    "attempt_id": "att_...",
    "kind": "metric_batch",
    "occurred_at": "2026-01-01T00:00:00Z",
    "payload": {}
  }]
}
```

The response durably acknowledges a cumulative sequence and supplies desired
commands:

```json
{
  "server_time": "2026-01-01T00:00:01Z",
  "accepted_through": 41,
  "next_sync_after": "10s",
  "commands": [{
    "command_id": "cmd_...",
    "kind": "offer_attempt",
    "attempt_id": "att_...",
    "lease_expires_at": "2026-01-01T00:02:01Z",
    "payload": {}
  }]
}
```

The worker records commands before acknowledging them. Command IDs and event
IDs are stable, so either side may repeat a lost response. The worker deletes
outbox rows only through `accepted_through`; partial batch acceptance is valid.
Payload size, event count, and sync duration are bounded.

## Metrics, logs, and artifacts

Metrics are timestamped scalar samples with a name, step, value, and small tag
set. The worker aggregates configured high-rate series before upload. Binary
payloads are never embedded in metric events.

Job specifications may declare metric presentation, optimization direction,
and named reference lines such as goals, benchmarks, baselines, and thresholds.
Percent metrics use fractional raw values from 0 through 1. Reference lines are
chart annotations; health policies remain the mechanism for automated actions.
See the [metrics guide](metrics.md) for the full contract, including the
reserved `lcjs/` prefix under which the worker reports automatic resource
series.

`GET /api/v1/jobs/{id}/metrics` returns each series' definition together with
its recorded points `(attempt, step, value, observed_at)` and a `cursor`.
Passing the cursor back as `?after=` returns only samples committed since, so
the console's live charts poll cheaply while a job runs. The browser console
reads the same data from `/jobs/{id}/metrics.json` under its session cookie.

Logs are announced as bounded chunks with stream, byte range, checksum, and
retention class. Small chunks may be uploaded; large logs use the artifact
transfer mechanism.

Artifact announcements contain stable ID, logical name, worker URI, byte size,
checksum, media type, and whether the artifact is a resumable checkpoint. When
the file is at most 64 KiB of valid UTF-8 text, the announcement also carries
its full bytes in an optional `content` string; larger or binary artifacts
announce metadata only. An artifact is not considered committed until it has
been atomically renamed into its final store path and its announcement is in
the worker outbox.

Each artifact in the job detail response includes its stored `content` when
the worker inlined it, so small text results are readable through the API and
previewed on the console job page without contacting the worker.

## Scope vocabulary

The initial scopes are deliberately coarse and may narrow before release:

- `jobs:read`, `jobs:write`
- `workers:read`, `workers:admin`
- `events:read`
- `tokens:write`
- `worker:sync:<worker-id>`
- `artifacts:request`

Authorization is deny-by-default. Scope checks happen in the domain operation,
not only in the HTTP router, so future CLI or internal callers cannot bypass
them.
