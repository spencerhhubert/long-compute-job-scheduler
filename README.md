# Long Compute Job Scheduler

A small, durable control plane for dispatching long-running compute jobs to a
heterogeneous set of machines. It is designed for ML experiments, CPU data
generation, temporary cloud workers, intermittent connectivity, and jobs whose
useful output may be much larger than the control-plane host.

The project is currently pre-alpha. The first vertical slice provides durable,
idempotent job submission, hashed operator tokens, persistent browser sessions,
authenticated central reads, and a compact operational dashboard. Worker
execution remains on the roadmap.

The authenticated console is available at
[compute-jobs.spencerhubert.info](https://compute-jobs.spencerhubert.info).

## Shape of the system

- One Go binary provides a CLI, control-plane server, and worker agent.
- The control plane owns desired state, scheduling, authentication, the JSON
  API, and a small HTMX dashboard.
- Every worker owns local execution state and a durable SQLite outbox.
- Workers initiate batched HTTPS syncs to the control plane. They do not need an
  inbound port, a VPN, or continuous connectivity.
- The control plane serves reads entirely from its own SQLite database. An
  offline worker cannot make the UI hang.
- Large outputs stay in a configured artifact store. The central database holds
  portable metadata and locations, not machine-specific absolute paths.

See [the architecture](docs/architecture.md), [API draft](docs/api.md), and
[implementation roadmap](docs/roadmap.md).

## Development quick start

Go 1.27 or newer is required.

```sh
go build -o bin/lcjs ./cmd/lcjs
export LCJS_BOOTSTRAP_TOKEN="$(bin/lcjs token)"
bin/lcjs server --listen 127.0.0.1:8080 --db data/control.db
```

In another shell, create an operator key and enter the value printed once at
`/login`:

```sh
bin/lcjs token create --db data/control.db --name primary-browser
```

The server stores only a SHA-256 hash of the operator key. A successful browser
login exchanges it for a random, server-side session and sets a 90-day Secure,
HttpOnly, SameSite=Strict cookie. The same key is accepted as an API bearer
token. The bootstrap token remains available for initial provisioning and
recovery. The server binds to loopback by default; terminate TLS in front of it
before exposing it publicly.

Run the full local check with:

```sh
make check
```

## Intended guarantees

- Accepted job submissions survive control-plane restarts.
- Accepted worker events survive worker restarts and are delivered at least
  once; the control plane applies them idempotently.
- An attempt is not silently duplicated just because a worker misses a
  heartbeat.
- Scheduling is conservative by default, including exclusive GPU allocation.
- Checkpoint-aware jobs can restart after machine failure from a recorded
  checkpoint. Arbitrary processes cannot transparently survive a machine
  reboot.
- Credentials are scoped and stored separately from job specifications and
  logs.

## Deliberate non-goals for the first release

- Replacing Kubernetes or a cloud provider's fleet manager
- Running mutually untrusted tenants on the same worker
- Requiring a particular VPN, reverse proxy, object store, or cloud service
- Streaming every metric or log line synchronously
- Transparent recovery of programs that do not checkpoint

## Contributing

Read [AGENTS.md](AGENTS.md) before making changes. The repository is intended to
be public; machine-specific notes and secrets must remain untracked.
