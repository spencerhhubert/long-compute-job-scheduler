# 0002: Use SQLite on the control plane and workers

- Status: accepted for initial implementation
- Date: 2026-08-22

## Context

The expected initial fleet is small, but durable restarts and simple operation
matter immediately. Adding a network database would increase operational cost
without removing the need for durable worker-local queues.

## Decision

Use one SQLite database on the control plane and one on each worker. Enable WAL,
foreign keys, and a busy timeout; keep transactions short; version all schema
changes; and expose database backup and integrity-check operations.

## Consequences

- Deployment and recovery stay simple and the same persistence model works on
  Linux and macOS.
- The control plane remains a single-writer service in the initial design.
- Metric ingestion needs batching, retention, and roll-ups.
- A later storage split must preserve the event and domain contracts rather than
  expose database tables as the API.
