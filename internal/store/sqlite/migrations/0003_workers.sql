CREATE TABLE workers (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    status         TEXT NOT NULL,
    agent_version  TEXT NOT NULL DEFAULT '',
    session_id     TEXT NOT NULL DEFAULT '',
    capacity_json  BLOB NOT NULL CHECK (json_valid(capacity_json)),
    last_seen_at   TEXT,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
) STRICT;

CREATE TABLE worker_tokens (
    token_hash    BLOB PRIMARY KEY CHECK (length(token_hash) = 32),
    worker_id     TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    created_at    TEXT NOT NULL,
    last_used_at  TEXT,
    revoked_at    TEXT
) STRICT;

CREATE TABLE attempts (
    id                TEXT PRIMARY KEY,
    job_id            TEXT NOT NULL REFERENCES jobs(id),
    attempt_number    INTEGER NOT NULL,
    worker_id         TEXT NOT NULL REFERENCES workers(id),
    command_id        TEXT NOT NULL UNIQUE,
    state             TEXT NOT NULL,
    revision          INTEGER NOT NULL,
    offered_at        TEXT NOT NULL,
    lease_expires_at  TEXT NOT NULL,
    accepted_at       TEXT,
    started_at        TEXT,
    finished_at       TEXT,
    exit_code         INTEGER,
    error             TEXT NOT NULL DEFAULT '',
    log_uri           TEXT NOT NULL DEFAULT '',
    UNIQUE(job_id, attempt_number)
) STRICT;

CREATE INDEX attempts_worker_state
    ON attempts(worker_id, state, offered_at);

CREATE INDEX attempts_job
    ON attempts(job_id, attempt_number);

CREATE TABLE worker_events (
    worker_id    TEXT NOT NULL REFERENCES workers(id),
    sequence     INTEGER NOT NULL,
    event_id     TEXT NOT NULL UNIQUE,
    attempt_id   TEXT NOT NULL REFERENCES attempts(id),
    kind         TEXT NOT NULL,
    payload_json BLOB NOT NULL CHECK (json_valid(payload_json)),
    occurred_at  TEXT NOT NULL,
    recorded_at  TEXT NOT NULL,
    PRIMARY KEY(worker_id, sequence)
) STRICT;

CREATE TABLE metric_samples (
    attempt_id   TEXT NOT NULL REFERENCES attempts(id),
    event_id     TEXT NOT NULL UNIQUE REFERENCES worker_events(event_id),
    name         TEXT NOT NULL,
    value        REAL NOT NULL,
    step         INTEGER,
    observed_at  TEXT NOT NULL,
    recorded_at  TEXT NOT NULL
) STRICT;

CREATE INDEX metric_samples_attempt_name
    ON metric_samples(attempt_id, name, observed_at);

CREATE TABLE artifacts (
    attempt_id   TEXT NOT NULL REFERENCES attempts(id),
    name         TEXT NOT NULL,
    uri          TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY(attempt_id, name, uri)
) STRICT;
