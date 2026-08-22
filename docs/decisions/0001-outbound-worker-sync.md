# 0001: Workers use outbound durable sync

- Status: accepted for initial implementation
- Date: 2026-08-22

## Context

Compute workers may be private, intermittently connected, or short-lived. The
dashboard must remain responsive without querying them. Events must survive
agent, network, and control-plane restarts.

## Decision

Each worker keeps a local SQLite inbox/outbox and initiates bounded HTTPS syncs
to the control plane. The control plane acknowledges events only after a durable
transaction and returns idempotent commands in the response. No inbound worker
port or mandatory VPN is required.

## Consequences

- Offline workers continue recording local state and catch up later.
- The central UI always shows a last-known projection and its freshness.
- Commands have polling latency; urgent cancellation is bounded by the sync
  interval rather than instantaneous.
- Large event batches, clock skew, duplicate delivery, and worker identity must
  be handled explicitly.
