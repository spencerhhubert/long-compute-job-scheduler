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
| Direct execution in project directories with recorded git state | Partial | Scheduling, git-state recording, and pinned-checkout rules have unit coverage; a live job under the new model has not been re-verified |
| Actual command execution and state reporting | Verified | Submitted fixtures reported start, exit code 0, metrics, artifacts, and terminal success |
| Restart and machine-loss recovery | Partial | A live supervisor survived an agent service restart and the new agent reported completion; reboot/lost-machine policy and checkpoint resume remain |
| Multiple workers and temporary cloud nodes | Missing | Same protocol successfully dispatches to two heterogeneous workers |
| Parallel jobs and GPU-aware scheduling | Partial | Reservation and capacity rules have unit coverage and the worker advertises parallel/GPU capacity; live overlap and GPU isolation remain unverified |
| Low orchestration overhead | Missing | CPU, memory, disk, and network overhead are measured during a representative compute job |

## Data, telemetry, and automation

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Automatic scalar telemetry | Verified | A real process emitted ordered scalar samples that survived local batching and appeared in the central job detail API |
| Metric charts and comparisons | Verified | An authenticated production job detail plotted real samples and a named benchmark on the same chart |
| Flatline/health-policy detection | Verified | A production fixture emitted 12 flat samples; the 10-second policy fired exactly one audited event |
| Alerts and targeted hooks | Partial | Four production notifications were HMAC-verified and durably delivered once with HTTP 204; the intended chat/session receiver still needs to be configured |
| Structured large-file/artifact storage | Verified | A declared result was atomically copied to the configured large-file volume, hashed, and centrally indexed by portable URI |
| Checkpoint-aware recovery | Missing | Interrupted fixture resumes a new attempt from the latest committed checkpoint |
| Logs available through UI and API | Verified | A production fixture's bounded log tail appeared through the authenticated API and job detail UI |

## Operator experience

| Requirement | Status | Acceptance test |
| --- | --- | --- |
| Compact light-first HTMX dashboard | Partial | Authenticated job/worker overview and job details use central attempts, metrics, goals, artifacts, logs, and health events; richer controls and final visual acceptance remain |
| Strong versioned API and CLI | Partial | Job submit/list/detail/wait/cancel plus worker provisioning, agent, and metric CLIs work; retry, token administration, and artifact retrieval remain |
| Automatic release/deployment | Partial | CI cross-builds immutable artifacts deployed to control and worker hosts; installation and upgrades are not yet automated |

## Current honest capability

The deployed service now assigns native jobs to an outbound worker, which runs
them directly in its mapped project directories and records the observed git
state per attempt; it durably reports attempts, metrics, bounded logs,
health-policy events, and filesystem artifacts, and renders those results in
the job detail UI. The clone-and-execute path survived an in-flight agent
restart before this model replaced it; the direct path has not been
re-verified live. Periodic and
metric-flatline policies can deliver durable signed notifications to named
webhook targets. The missing and partial rows above remain real limitations;
in particular, machine-loss/checkpoint recovery, automatic cancel/retry health
actions, a configured chat/session receiver, multi-worker verification, and
remote artifact retrieval are not complete.
