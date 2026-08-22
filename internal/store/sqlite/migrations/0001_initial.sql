CREATE TABLE jobs (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    name        TEXT NOT NULL,
    state       TEXT NOT NULL,
    priority    INTEGER NOT NULL,
    revision    INTEGER NOT NULL,
    spec_json   BLOB NOT NULL CHECK (json_valid(spec_json)),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

CREATE INDEX jobs_queue_order
    ON jobs(state, priority DESC, created_at, id);

CREATE TABLE events (
    sequence     INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT NOT NULL UNIQUE,
    kind         TEXT NOT NULL,
    resource_id  TEXT,
    payload_json BLOB NOT NULL CHECK (json_valid(payload_json)),
    occurred_at  TEXT NOT NULL,
    recorded_at  TEXT NOT NULL
) STRICT;

CREATE INDEX events_resource_sequence
    ON events(resource_id, sequence);

CREATE TABLE idempotency_keys (
    operation     TEXT NOT NULL,
    key           TEXT NOT NULL,
    request_hash  BLOB NOT NULL,
    resource_id   TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (operation, key)
) STRICT;
