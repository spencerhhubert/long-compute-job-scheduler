# Worker setup and execution

Workers connect outbound to the control plane over HTTPS. They require no
inbound port and no VPN. Each worker has a credential bound to its worker ID;
that credential cannot call operator APIs.

Status: this is the first native-executor slice. It supports immutable Git
revisions, argv commands, environment values, scalar metrics, logs, filesystem
artifacts, conservative CPU/memory/GPU reservations, and durable local command
and event queues. OCI images, secret providers, cancellation, and remote
artifact transfer remain unimplemented and are tracked in
[the requirements checklist](requirements.md).

## Provision a credential

On the control-plane host, create a worker record against the control database:

```sh
lcjs worker create \
  --db /var/lib/lcjs/control.db \
  --name gpu-worker-1 \
  --label os=linux \
  --label cuda=true
```

The command prints `LCJS_WORKER_ID` and `LCJS_WORKER_TOKEN` exactly once. Put
those two lines in `/etc/lcjs-worker/worker.env`, owned by root with mode 0600.
The database stores only the token hash.

## Run the agent

Create a dedicated service user and two writable roots: a smaller state/work
root and the configured large artifact root. Install the CI-built `lcjs` binary
and adapt [`deploy/lcjs-worker.service`](../deploy/lcjs-worker.service), in
particular the control URL, resource flags, labels, and paths.

Useful resource flags are:

```text
--cpu N
--memory-bytes N
--gpus N
--gpu-shared-slots N
--max-parallel N
--label name=value
```

GPU jobs are exclusive by default. Shared jobs are eligible to overlap only
when their job request uses `sharing: "shared"` and the worker explicitly
advertises enough `--gpu-shared-slots`. Shared slots are reservations, not
performance isolation.

The service template uses `KillMode=process`: restarting the agent leaves its
supervisor children running, and the new agent reads their atomic status files.
To intentionally terminate the entire service cgroup, use an explicit
all-process kill before stopping the unit. Jobs cannot survive a machine
reboot; after reboot the agent reports a missing supervisor and the job retry
policy decides whether to create a new attempt.

## Job runtime contract

The worker initializes an isolated directory, fetches the exact full Git object
ID, checks out detached `FETCH_HEAD`, and executes the command as an argv array
without a shell. It supplies:

```text
LCJS_JOB_ID
LCJS_ATTEMPT_ID
LCJS_ATTEMPT_NUMBER
LCJS_METRICS_FILE
LCJS_ARTIFACT_DIR
```

Standard output and error are combined in a permission-restricted `run.log`
under the attempt artifact directory. Exit status is written atomically by a
separate supervisor process.

To emit a declared scalar metric from a shell command:

```sh
lcjs metric --name validation/accuracy --value 0.947 --step 1200
```

Libraries may instead append one JSON object per line to `LCJS_METRICS_FILE`
using the schema in [the metrics guide](metrics.md). A newline terminates and
commits a sample; the worker does not advance its durable read offset for a
partial line.

Artifact globs are evaluated relative to the checkout after the process exits.
Matching regular files are copied atomically into:

```text
<artifact-root>/<project>/<job-id>/<attempt-number>/<artifact-name>/...
```

The control plane receives only portable `worker://` URIs, sizes, and SHA-256
checksums. Absolute worker paths never cross the protocol boundary.
