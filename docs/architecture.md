# Architecture

Status: design draft

## Topology

```text
 browser / CLI / automation
            |
         HTTPS API
            v
 +-------------------------+
 | control plane           |
 | scheduler + HTMX UI     |
 | central SQLite          |
 +-------------------------+
            ^
            | outbound HTTPS sync, batched and retryable
            |
 +-------------------------+       +-------------------------+
 | worker agent            |  ...  | worker agent            |
 | local SQLite + executor |       | local SQLite + executor |
 +-------------------------+       +-------------------------+
       |             |                   |             |
   processes    artifact store       processes    artifact store
```

There is one logical control plane and any number of workers. A worker may be a
permanent GPU machine, a workstation that is only sometimes online, or a
short-lived cloud instance. The same protocol applies to all of them.

Workers initiate every required network connection. The design does not depend
on the control plane being able to dial a worker, which avoids exposing a
private compute machine and removes VPN enrollment from the normal worker setup.

## The durability boundary

Both sides use SQLite, but for different purposes:

| State | Authority | Replication rule |
| --- | --- | --- |
| Job specification and desired state | Control plane | Never inferred from a worker |
| Assignment and lease | Control plane | Included in every relevant sync response |
| Actual attempt state | Worker while connected to the attempt | Sent as ordered events |
| Unacknowledged events and metrics | Worker outbox | Deleted only through an explicit cumulative acknowledgement |
| Historical events and UI projections | Control plane | Written transactionally before acknowledgement |
| Artifact bytes | Configured artifact store | Not placed in either SQLite database |
| Artifact metadata | Worker, then control plane | Identified by a stable artifact ID and relative location |

Each worker has a stable ID and each agent start has a fresh session ID. Outbox
records have a monotonically increasing sequence within one agent session. The
control plane uniquely indexes `(worker_id, session_id, sequence)`, so a
repeated sync is harmless and an agent restarted with fresh local state begins
a new session instead of colliding with recorded history; event IDs stay
globally unique for deduplication.

SQLite runs in WAL mode with foreign keys enabled and a busy timeout. Schema
migrations are monotonic and run before either service accepts work, applied
with foreign keys off and verified with a full foreign-key check before
enforcement is re-enabled, so a migration can rebuild a table that other
tables reference. Control-
plane backups use SQLite's online backup mechanism or `VACUUM INTO`, not raw
copies of a live multi-file database.

## Job and attempt model

A **job** is the durable user request: project, command, optional pinned
revision, resource request, environment references, retry policy, artifact
rules, and health policies. An **attempt** is one execution of that job on one
worker.

Job state:

```text
queued -> running -> succeeded
   |         |  \
   |         |   -> canceling -> canceled
   |         -> retry_wait -> queued
   +---------------------------> canceled
                         failures exhausted -> failed
```

Attempt state:

```text
offered -> accepted -> starting -> running -> succeeded
    |          |          |          |  \
    |          |          |          |   -> checkpointing
    |          |          |          -> failed / canceled
    |          |          -> failed
    |          -> rejected / expired
    -> expired
```

State transitions use compare-and-swap semantics: each job and attempt carries a
revision, and a transition names the revision it observed. Terminal transitions
are immutable. Administrative corrections are new audit events, not edits to
history.

### Leases and uncertainty

An assignment is a renewable lease, not proof that a process is running. When a
worker stops syncing, the control plane marks the attempt `unknown` after a
grace period. It does not immediately launch a duplicate. After a configurable
lost timeout, the job's retry policy decides whether to fail, wait for operator
review, or create another attempt.

This bias matters for expensive jobs and external side effects. Jobs that are
safe to duplicate can opt into faster reassignment.

## Scheduling

Workers periodically report a resource snapshot:

- CPU capacity and allocatable units
- memory and local scratch space
- OS, architecture, and operator-defined labels
- GPUs by stable device identifier, model, memory, and supported capabilities
- reservations held by active attempts

Jobs request resources and optional label constraints. The first scheduler uses
priority plus FIFO ordering and a deterministic best-fit score. It never uses
observed utilization as if it were allocatable capacity; reservations are the
scheduling authority.

GPU allocation is exclusive by default. A worker may explicitly enable shared
GPU slots with declared memory reservations. This permits independent CUDA
processes to overlap, but the scheduler must not promise performance isolation.
MIG, MPS, and provider-specific GPU partitioning belong behind later capability
adapters rather than in the core job model.

Distributed training requires atomic allocation of a group of tasks (gang
scheduling), rendezvous data, and failure policy for the whole group. The data
model will retain a task/group boundary, but this is not part of the first
single-worker execution slice.

## Execution and deployment

The portable job specification identifies:

- the project, which selects the worker directory the job runs in
- command/arguments and environment values
- an optional pinned commit to check out before running
- named secret references, never secret values
- requested resources and worker constraints
- checkpoint, artifact, telemetry, and retry behavior

Workers map project names to local directories and advertise those names; the
scheduler offers a job only to workers advertising its project. The native
executor starts a supervised process group directly in the project directory,
as the agent's own OS user, and records the directory's git state (commit,
branch, dirty flag) on the attempt. Reproducibility is observed and recorded
rather than enforced; a pinned commit is checked out only when the directory
is a clean git tree. Container and provider-specific executors can be added
without changing the scheduler protocol.

Agent restart and machine restart are separate cases. A worker may reattach to
a still-running supervised process after its own restart. Following a machine
reboot, recovery means starting a new attempt with a job-defined resume command
and the latest committed checkpoint.

## Artifacts

An artifact store is configured per worker. The default filesystem layout is:

```text
<artifact-root>/<project>/<job-id>/<attempt-number>/<artifact-name>
```

Database rows store a URI such as
`worker://<worker-id>/<project>/<job-id>/<attempt>/<name>`, a byte size, media
type, checksum, creation time, and optional checkpoint metadata. Absolute host
paths never cross the protocol boundary.

The dashboard lists artifacts from central metadata. Fetching large bytes is a
separate, explicit transfer operation. A future transfer adapter may copy to an
object store, establish a short-lived reverse transfer, or provide an
operator-local retrieval command; ordinary page loads never touch artifact
bytes.

## Telemetry and automated health policies

The worker batches structured events, scalar metrics, log metadata, and artifact
announcements into its outbox. High-rate metrics are aggregated into bounded
time buckets before upload. Logs are chunked and subject to retention limits.

A job can declare policies such as:

- heartbeat missing for a duration
- process exited or emitted NaN/Inf
- a named metric has not improved by a threshold over a window
- wall-clock, cost, or step budget exceeded
- free disk or GPU memory below a threshold

Each policy has an explicit action: annotate, notify, request checkpoint then
stop, cancel immediately, or retry. Metric semantics remain job-defined; the
orchestrator does not guess whether a training curve has plateaued.

Notifications consume durable control-plane events and have their own retrying
outbox. Webhooks are the first generic destination, allowing a targeted agent
session, chat bot, pager, or custom automation to be attached without coupling
those systems to the worker.

## Authentication and trust

- HTTPS is mandatory outside loopback development.
- API tokens contain at least 256 bits of randomness and are shown once. Only a
  cryptographic hash is stored.
- Browser login exchanges an operator token for a separate 256-bit session
  secret. Only its hash is persisted; the browser receives a Secure, HttpOnly,
  SameSite=Strict cookie with a bounded lifetime.
- Tokens have a subject, scopes, optional worker binding, expiry, and revocation
  time.
- A short-lived enrollment token may create exactly one worker credential.
- Worker credentials can sync only their own identity and cannot submit jobs or
  read secrets for another worker.
- Job secrets are resolved on the chosen worker through a secret-provider
  interface and are redacted from manifests, events, logs, and the UI.
- Webhook deliveries are signed and contain stable event IDs for deduplication.

The first release assumes trusted operators and trusted jobs. Sandboxing hostile
workloads is a separate problem.

## Responsiveness and scaling limits

The dashboard and public API read central projections only. Worker sync is
bounded by request size and transaction duration. Metrics have retention and
roll-up policies so they cannot grow the primary database without limit.

One Go process and SQLite are intentionally sufficient for the initial scale.
The event protocol creates an escape hatch: projections, metric storage, or the
scheduler can later move behind the same logical API if measured load requires
it. No distributed component is introduced preemptively.
