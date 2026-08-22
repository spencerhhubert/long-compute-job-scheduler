# Repository guidance

This repository is intended to become public. Treat every tracked file, commit
message, fixture, example, log excerpt, and generated artifact as public.

## Public-safety boundary

- Never commit secrets, tokens, private URLs, personal information, machine
  names, private network details, absolute host paths, or environment-specific
  credentials.
- Use neutral, professional language in public documentation and comments.
- Use placeholders such as `control.example.com`, `gpu-worker-1`, and
  `/srv/lcjs/artifacts` in tracked examples.
- Put machine-specific notes in `AGENTS.local.md`, which is gitignored. Never
  weaken that ignore rule.
- Before committing, inspect both the staged diff and newly added files for
  information that should remain private.

## Engineering principles

- Keep the control plane responsive when every worker is slow or offline. A
  dashboard request must never synchronously query a worker.
- Persist state before acknowledging it. Cross-machine operations must be
  idempotent and safe to retry.
- Workers initiate outbound HTTPS connections; workers do not require inbound
  internet access.
- Prefer explicit state machines and append-only events to hidden behavior.
- Keep orchestration overhead small relative to the jobs being managed.
- Make safe behavior the default: exclusive GPU allocation, bounded retries,
  redacted logs, scoped credentials, and conservative lost-worker handling.
- Treat arbitrary process restart and checkpoint-based job recovery as
  different guarantees. Never claim a process can survive a machine reboot.
- Keep domain and protocol types independent from SQLite, HTTP, and the UI.
- Add tests for state transitions, idempotency, migrations, scheduling, and
  authentication changes.
- Keep third-party dependencies few, pinned, and justified.

## Working conventions

- The primary implementation language is Go.
- The browser UI is server-rendered HTML enhanced with HTMX. Core operations
  must also be available through a versioned JSON API and CLI.
- SQLite is the durable store on both the control plane and each worker unless
  an accepted design record says otherwise.
- Run `make check` before committing. Update architectural documentation when a
  change alters a protocol, persistence guarantee, or trust boundary.
- Do not preserve compatibility before the first tagged release unless a task
  explicitly requires it; favor a coherent API while the design is young.

Read `AGENTS.local.md` when it exists for untracked environment-specific notes.
