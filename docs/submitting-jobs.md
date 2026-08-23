# Submitting ML jobs

This is the shortest handoff for an operator or agent preparing a batch of
experiments. A job names a project and an argv command. It runs in the project
directory the assigned worker maps to that project name, so datasets, caches,
virtual environments, checkpoints, and results live where that machine already
keeps them. The git state of the directory (commit, branch, dirty flag) is
recorded on every attempt.

## 1. Install credentials

Set the control URL and operator token in the submitting shell:

```sh
export LCJS_URL=https://control.example.com
export LCJS_TOKEN=lcjs_...
```

The token must not be put in the repository, job JSON, command arguments, or
logs.

## 2. Define one job

Save a document such as `experiment-a.json`:

```json
{
  "project": "vision-study",
  "name": "resnet-seed-01",
  "priority": 0,
  "command": ["python3", "train.py", "--config", "configs/a.toml", "--seed", "1"],
  "environment": {"PYTHONUNBUFFERED": "1"},
  "resources": {
    "cpu": 4,
    "memory_bytes": 12000000000,
    "gpus": [{"count": 1, "sharing": "exclusive"}]
  },
  "constraints": {"labels": {"cuda": "true"}},
  "retry": {"max_attempts": 2, "lost_worker": "manual"},
  "checkpoint": {
    "glob": "checkpoints/*.pt",
    "resume_command": ["python3", "train.py", "--resume", "${LCJS_CHECKPOINT}"]
  },
  "artifacts": [
    {"name": "checkpoints", "glob": "checkpoints/*.pt"},
    {"name": "results", "glob": "results/*.json"}
  ],
  "metrics": [
    {
      "name": "validation/accuracy",
      "display_name": "Validation accuracy",
      "format": "percent",
      "precision": 1,
      "objective": "maximize",
      "reference_lines": [
        {"kind": "benchmark", "label": "Human best", "value": 0.945},
        {"kind": "goal", "label": "Project goal", "value": 0.960}
      ]
    },
    {
      "name": "validation/loss",
      "display_name": "Validation loss",
      "format": "number",
      "precision": 4,
      "objective": "minimize"
    }
  ],
  "health": [
    {"kind": "periodic", "window": "1h", "action": "notify", "target": "research-session"},
    {"kind": "metric_stalled", "metric": "validation/accuracy", "mode": "max", "window": "45m", "minimum_delta": 0.001, "action": "notify", "target": "research-session"}
  ]
}
```

`project` decides where the job runs: only workers that map a directory for
that project name are offered the job, and the command's working directory is
that mapped directory.

To pin the exact revision, add an optional source:

```json
"source": {"commit": "0123456789abcdef0123456789abcdef01234567"}
```

The value must be the full immutable object ID, not a branch name. The worker
checks it out before starting the command, and only when the project directory
is a clean git tree; otherwise the attempt fails. Without a pin the job runs
at whatever state the directory is in, and the recorded git state on the
attempt tells you what that was.

## 3. Report metrics from training

LCJS supplies `LCJS_METRICS_FILE` to the process. Call the installed helper at
meaningful evaluation checkpoints:

```sh
lcjs metric --name validation/accuracy --value 0.947 --step 1200
lcjs metric --name validation/loss --value 0.1832 --step 1200
```

Python may write the same newline-delimited JSON directly. Metric names and raw
scales must match the declarations; percentages use fractions from zero to one.

## 4. Submit and wait

```sh
lcjs job submit \
  --server "$LCJS_URL" \
  --file experiment-a.json \
  --idempotency-key vision-study-resnet-seed-01
```

The response contains the stable job ID. Repeating the same key and document is
safe. Reusing the key for a changed document is rejected.

```sh
lcjs job get --server "$LCJS_URL" --id job_...
lcjs job wait --server "$LCJS_URL" --id job_...
lcjs job list --server "$LCJS_URL" --limit 100
```

Use a different job name and idempotency key for each seed/configuration. A
small generator may produce a directory of JSON documents and submit them in a
loop; the scheduler owns queuing, capacity reservations, and worker placement.

## Current boundaries

- Native-process execution in worker project directories is working; OCI
  images and arbitrary directory uploads are not.
- Metrics, recent logs, and filesystem artifacts are centrally visible.
- Agent restart recovery is verified. Machine-reboot checkpoint resume is not
  complete yet, so `lost_worker: "manual"` is the conservative choice for
  expensive or non-idempotent work.
- Health webhooks notify durably. Automatic cancel/retry actions are not yet
  enabled.
- Secret references are reserved in the contract but no worker secret provider
  is configured yet.
