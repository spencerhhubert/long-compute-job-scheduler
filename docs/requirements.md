# Product requirements and verified status

This is the authoritative acceptance checklist for the project. A requirement
is marked complete only after its real path has been exercised. A schema,
mockup, document, or dashboard placeholder is not a completed capability.

Status key:

- **Verified:** implemented and exercised through the real boundary
- **Partial:** useful implementation exists, but the requested capability does
  not yet work end to end
- **Missing:** not implemented

## Control plane and security

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Public, environment-neutral repository | Verified | Tracked files contain no machine-specific paths, private host details, or credentials |
| Always-available Go control plane with central SQLite | Verified | HTTPS service survives restart and retains accepted jobs |
| Strong operator authentication | Partial | Hashed operator tokens and browser sessions work; granular operator scopes and revocation UI remain |
| Scoped credentials for new compute nodes without an interactive VPN login | Missing | Mint one-use enrollment credential, enroll a worker, rotate it, and prove it cannot access operator APIs |
| Responsive reads independent of worker availability | Verified | Dashboard and API read central SQLite while all workers are offline |

## Dispatch and execution

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Durable job queue | Partial | Submission is durable and idempotent, but no worker can currently claim a job |
| Outbound worker communication | Missing | A private worker connects outward over HTTPS, receives work, and needs no inbound port |
| Worker-local SQLite inbox/outbox | Missing | Commands survive worker restart and events are retried without duplication |
| Automatic source deployment/materialization | Missing | Worker checks out the exact requested commit in an isolated work directory |
| Actual command execution and state reporting | Missing | Submitted fixture transitions queued → running → succeeded with captured exit status |
| Restart and machine-loss recovery | Missing | Agent restart preserves/rattaches live work; machine loss follows retry/checkpoint policy without silent duplication |
| Multiple workers and temporary cloud nodes | Missing | Same protocol successfully dispatches to two heterogeneous workers |
| Parallel jobs and GPU-aware scheduling | Missing | Reservations prevent over-allocation while configured shared slots can run independent jobs concurrently |
| Low orchestration overhead | Missing | CPU, memory, disk, and network overhead are measured during a representative compute job |

## Data, telemetry, and automation

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Automatic scalar telemetry | Partial | Metric definitions, objectives, and reference lines are stored; sample ingestion does not exist |
| Metric charts and comparisons | Missing | Dashboard plots samples and named goals/benchmarks on the same chart |
| Flatline/health-policy detection | Missing | A stalled fixture triggers exactly one configured action and audit event |
| Alerts and targeted hooks | Missing | Durable webhook delivery retries safely and can target the intended external session |
| Structured large-file/artifact storage | Partial | Job contract and portable URI design exist; workers do not collect or publish files |
| Checkpoint-aware recovery | Missing | Interrupted fixture resumes a new attempt from the latest committed checkpoint |
| Logs available through UI and API | Missing | Live/recent bounded logs are centrally indexed without blocking on an offline worker |

## Operator experience

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Compact light-first HTMX dashboard | Partial | Authenticated read-only job table is live; operational worker, attempt, metric, log, and artifact views remain |
| Strong versioned API and CLI | Partial | Create/list/get job API and provisioning CLI exist; execution and administration operations remain |
| Automatic release/deployment | Partial | CI cross-builds artifacts and the control plane is deployed from them; worker installation and upgrades remain |

## Current honest capability

The deployed service is a durable authenticated job registry. It does **not**
currently execute submitted jobs. Every job remains queued until the worker,
sync, scheduler, and executor requirements above are implemented and verified.
