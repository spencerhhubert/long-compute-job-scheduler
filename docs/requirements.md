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
| Scoped credentials for new compute nodes without an interactive VPN login | Partial | Worker-bound credentials are hashed and cannot access operator APIs; one-use enrollment and rotation remain |
| Responsive reads independent of worker availability | Verified | Dashboard and API read central SQLite while all workers are offline |

## Dispatch and execution

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Durable job queue | Verified | A public-API fixture was assigned once and transitioned queued → running → succeeded |
| Outbound worker communication | Verified | A private worker connected outward over public HTTPS and received work without an inbound port or VPN |
| Worker-local SQLite inbox/outbox | Verified | Commands, metric offsets, and events persisted locally; acknowledged events drained only after central commit |
| Automatic source deployment/materialization | Verified | Worker fetched and checked out the requested full commit in an isolated attempt directory |
| Actual command execution and state reporting | Verified | Two submitted fixtures reported start, exit code 0, metrics, artifacts, and terminal success |
| Restart and machine-loss recovery | Partial | A live supervisor survived an agent service restart and the new agent reported completion; reboot/lost-machine policy and checkpoint resume remain |
| Multiple workers and temporary cloud nodes | Missing | Same protocol successfully dispatches to two heterogeneous workers |
| Parallel jobs and GPU-aware scheduling | Partial | Reservation and capacity rules have unit coverage and the worker advertises parallel/GPU capacity; live overlap and GPU isolation remain unverified |
| Low orchestration overhead | Missing | CPU, memory, disk, and network overhead are measured during a representative compute job |

## Data, telemetry, and automation

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Automatic scalar telemetry | Verified | A real process emitted ordered scalar samples that survived local batching and appeared in the central job detail API |
| Metric charts and comparisons | Missing | Dashboard plots samples and named goals/benchmarks on the same chart |
| Flatline/health-policy detection | Missing | A stalled fixture triggers exactly one configured action and audit event |
| Alerts and targeted hooks | Missing | Durable webhook delivery retries safely and can target the intended external session |
| Structured large-file/artifact storage | Verified | A declared result was atomically copied to the configured large-file volume, hashed, and centrally indexed by portable URI |
| Checkpoint-aware recovery | Missing | Interrupted fixture resumes a new attempt from the latest committed checkpoint |
| Logs available through UI and API | Missing | Live/recent bounded logs are centrally indexed without blocking on an offline worker |

## Operator experience

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Compact light-first HTMX dashboard | Partial | Authenticated job/worker overview and attempt/metric/artifact details are backed by central state; recent logs and richer controls remain |
| Strong versioned API and CLI | Partial | Create/list/detail/cancel APIs plus worker provisioning, agent, and metric CLIs work; retry, token administration, and artifact retrieval remain |
| Automatic release/deployment | Partial | CI cross-builds immutable artifacts deployed to control and worker hosts; installation and upgrades are not yet automated |

## Current honest capability

The deployed service now assigns and executes native Git-based jobs on an
outbound worker, durably reports attempts and scalar metrics, and indexes
filesystem artifacts. The working path has survived an in-flight agent restart.
The missing and partial rows above remain real limitations; in particular,
machine-loss/checkpoint recovery, health actions, targeted alerts, multi-worker
verification, centralized recent logs, and remote artifact retrieval are not
complete.
