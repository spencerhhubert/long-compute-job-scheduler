# Implementation roadmap

The project should grow as tested vertical slices. A slice is complete only when
its restart and retry behavior has been exercised, not merely when its happy
path works.

## 0. Contract and repository foundation

- Public repository guidance and untracked local notes
- Architecture, state ownership, protocol draft, and explicit non-goals
- Go module, lint/test/build commands, and CI
- Decision records for outbound worker sync and SQLite durability

## 1. Durable control plane

- SQLite migrations and backup command
- Scoped token authentication with a one-time bootstrap path
- Submit, list, inspect, cancel, and retry jobs through JSON and CLI
- Append-only event audit and idempotency keys
- Server-rendered read-only job list

Acceptance: kill the service after every transaction boundary in an integration
test; accepted jobs remain visible and duplicate submissions remain singular.

## 2. One durable worker

- Enrollment and credential rotation
- Local SQLite command inbox and event outbox
- Resource discovery, bounded sync, leases, and unknown-worker handling
- Native process executor with process-group cancellation and log chunking
- System service examples for Linux and macOS

Acceptance: run a fixture job while repeatedly restarting the worker and
control plane and intermittently blocking the network. It completes once, with
an ordered central history and no lost accepted events.

## 3. Reproducibility, artifacts, and checkpoints

- Immutable git revision materialization and execution manifests
- Configurable filesystem artifact store and portable artifact URIs
- Checksummed artifact discovery and retention
- Job-defined checkpoint and resume flow
- Explicit on-demand transfer requests

Acceptance: interrupt a checkpointing fixture with a simulated machine loss,
then complete a new attempt from the last committed checkpoint.

## 4. Scheduling and heterogeneous resources

- CPU, memory, scratch, labels, and exclusive GPU reservations
- Priority/FIFO best-fit scheduler with explainable decisions
- Optional worker-declared GPU sharing slots
- NVIDIA telemetry adapter with graceful absence on non-NVIDIA workers

Acceptance: property tests never over-allocate declared resources; a mixed test
fleet produces deterministic placements and clear unschedulable reasons.

## 5. Telemetry, policies, and notifications

- Job-defined metric presentation, objectives, and named reference lines
- Metric batching, roll-ups, and retention
- Health-policy evaluator with dry-run explanations
- Durable signed webhook delivery with retry and dead-letter visibility
- Actions for notify, checkpoint-and-stop, cancel, and retry

Acceptance: duplicate metrics and webhook attempts are harmless; a stalled
fixture triggers exactly one logical action and retains an audit trail.

## 6. Operator UI and deployment polish

- Minimal HTMX job, worker, attempt, metric, log, and artifact views
- Cross-compiled release binaries and checksums
- Non-interactive worker install and enrollment flow
- Documented reverse-proxy/TLS setup without a required vendor
- Upgrade, rollback, database backup, and restore runbooks

## Later, only when demanded by real workloads

- Atomic multi-task allocation for distributed training
- OCI/container executor
- MIG/MPS/provider-specific allocation adapters
- External object-store adapters
- Alternative metric storage or event consumers
- Multi-control-plane availability
