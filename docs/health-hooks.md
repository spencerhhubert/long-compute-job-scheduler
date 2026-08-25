# Health policies and targeted webhooks

LCJS can evaluate periodic check-ins and metric-stall policies while an attempt
is running. A policy targets a named webhook configured separately from the job
specification, so job JSON contains neither the destination URL nor its signing
secret.

Status: durable signed `notify` delivery is implemented. Webhook failures use
exponential backoff and become visible as `dead` after ten attempts. Automatic
cancellation, checkpoint-and-stop, and retry actions are not implemented yet.

## Configure a target

Run this on the control-plane host:

```sh
lcjs hook create \
  --db /var/lib/lcjs/control.db \
  --name research-session \
  --url https://automation.example.com/lcjs
```

The command prints `LCJS_WEBHOOK_TARGET` and `LCJS_WEBHOOK_SECRET` exactly once.
Store the secret at the receiver. The control database is permission-restricted
because it must retain the secret to sign future deliveries.

The receiver must:

1. calculate HMAC-SHA-256 over the exact raw request body;
2. compare it in constant time with `X-LCJS-Signature`, formatted as
   `sha256=<lowercase hex>`;
3. deduplicate the stable `X-LCJS-Event-ID` before causing an external action;
4. return any 2xx response only after it has durably accepted the notification.

`X-LCJS-Delivery-ID` identifies one retrying delivery. Receivers should dedupe
by event ID because a control-plane crash after a successful request but before
its local commit can repeat a delivery.

## Periodic check-in

This policy notifies the named target after every completed interval while the
attempt remains running:

```json
{
  "kind": "periodic",
  "window": "1h",
  "action": "notify",
  "target": "research-session"
}
```

Put it in the job's top-level `health` array. A stable interval key prevents a
control restart or repeated evaluation from creating duplicate logical events.

## Metric flatline

This example fires once per attempt when validation accuracy has improved by
less than `0.001` across the samples in a 45-minute window:

```json
{
  "kind": "metric_stalled",
  "metric": "validation/accuracy",
  "mode": "max",
  "window": "45m",
  "minimum_delta": 0.001,
  "action": "notify",
  "target": "research-session"
}
```

Use `mode: "min"` for a metric such as loss. The policy waits until the attempt
has run for the complete window and requires at least two samples inside that
window. The event records the observed delta, sample count, configured rule,
job, attempt, worker, and evaluation time.

## Attempt finished

This policy notifies the named target once when the attempt reaches a terminal
state, whatever that state is:

```json
{
  "kind": "finished",
  "action": "notify",
  "target": "research-session"
}
```

It takes no window or metric. The delivery body carries `attempt_state`
(`succeeded`, `failed`, or `canceled`), `exit_code`, and `job_state` at the
payload root, so a receiver can distinguish a final failure from one that will
be retried (`job_state` is `queued` while retries remain).

## Delivery body

The signed JSON body has stable resource IDs and the complete policy:

```json
{
  "event_id": "hlf_...",
  "kind": "health_policy_fired",
  "job": {"id": "job_...", "project": "example", "name": "train-a"},
  "attempt": {"id": "att_...", "number": 1, "worker_id": "wrk_..."},
  "policy_index": 0,
  "policy": {},
  "reason": "metric validation/accuracy improved by ...",
  "observed_at": "2026-01-01T00:00:00Z"
}
```

The job detail API and console show each logical firing, its target, delivery
state, retry count, response code, and last error.
