# Worker setup and execution

Workers connect outbound to the control plane over HTTPS. They require no
inbound port and no VPN. Each worker has a credential bound to its worker ID;
that credential cannot call operator APIs.

Status: the native executor runs jobs directly in per-project worker
directories. It supports argv commands, environment values, optional pinned
git commits, recorded git state, scalar metrics, logs, filesystem artifacts,
conservative CPU/memory/GPU reservations, and durable local command and event
queues. OCI images, secret providers, cancellation, and remote artifact
transfer remain unimplemented and are tracked in
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

## Project directories

The agent runs each job in a directory it already has for that job's project,
declared with a repeatable flag:

```text
--project example-research=/srv/projects/example-research
```

The name is the job spec's `project`; the path is an absolute directory on the
worker. The agent advertises its project names to the control plane, and a job
is only ever offered to a worker that advertises its project. A job whose
project no worker advertises stays queued until one does; it does not fail.

Attempts execute with the project directory as the working directory, as the
OS user the agent runs as, inheriting the agent's process environment plus the
job's `environment` overlay. Nothing is cloned or copied: the directory is
whatever checkout, virtual environment, and data layout that machine already
uses for the project.

## Run the agent

Choose the user the agent should run as: attempts run as that user and use its
environment, so it must own or share the project directories. Give it a
writable state directory and the configured large artifact root. Install the
CI-built `lcjs` binary and adapt
[`deploy/lcjs-worker.service`](../deploy/lcjs-worker.service), in particular
the control URL, project directories, resource flags, labels, and paths.

Useful flags are:

```text
--project name=/absolute/path
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

## macOS workers

A worker does not have to be Linux; CI builds `lcjs-darwin-arm64` and
`lcjs-darwin-amd64` alongside the Linux binaries. Three things differ:

- The downloaded binary carries a quarantine attribute and will not execute
  until it is removed: `xattr -d com.apple.quarantine /path/to/lcjs`.
- There is no `systemd`. Use a `launchd` LaunchAgent if the worker should
  survive a reboot; running it under `nohup` gets it up but does not.
- `timeout(1)` does not exist, so job commands that assume it will fail.

Run the agent with `--gpus 0` unless the machine really can offer one to a
job. An Apple GPU is not automatically usable by a framework that needs
float64: Metal has no `double` type at all, so a job whose stack requires it
must be told to use the CPU through its own configuration.

The service template uses `KillMode=process`: restarting the agent leaves its
supervisor children running, and the new agent reads their atomic status files.
To intentionally terminate the entire service cgroup, use an explicit
all-process kill before stopping the unit. Jobs cannot survive a machine
reboot; after reboot the agent reports a missing supervisor and the job retry
policy decides whether to create a new attempt.

## Job runtime contract

The worker executes the command as an argv array without a shell, in the
project directory. At attempt start it records the directory's git state into
the attempt: the current `HEAD`, the branch name (empty when detached), and a
dirty flag covering uncommitted changes to tracked files. The job API returns
this state and the console shows it. Reproducibility is recorded, not
enforced.

If the job spec pins a `source.commit`, the worker checks that commit out
first, and only when the project directory is a clean git tree; a dirty tree,
a missing repository, or an unknown commit fails the attempt with a clear
error. With a `source.git_url` the worker fetches the commit from that URL
when it is not already present locally.

The worker supplies:

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

Artifact globs are evaluated relative to the project directory after the
process exits. Matching regular files are copied atomically into:

```text
<artifact-root>/<project>/<job-id>/<attempt-number>/<artifact-name>/...
```

The control plane receives only portable `worker://` URIs, sizes, and SHA-256
checksums. When a matched file is at most 64 KiB of valid UTF-8 text, the
announcement also carries its full content so the console can preview it;
larger or binary artifacts announce metadata only. Absolute worker paths never
cross the protocol boundary.
