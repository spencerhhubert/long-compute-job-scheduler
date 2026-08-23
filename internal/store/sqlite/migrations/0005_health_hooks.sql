CREATE TABLE webhook_targets (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    url         TEXT NOT NULL,
    secret      BLOB NOT NULL CHECK (length(secret) >= 32),
    created_at  TEXT NOT NULL,
    revoked_at  TEXT
) STRICT;

CREATE TABLE health_firings (
    id            TEXT PRIMARY KEY,
    job_id        TEXT NOT NULL REFERENCES jobs(id),
    attempt_id    TEXT NOT NULL REFERENCES attempts(id),
    policy_index  INTEGER NOT NULL,
    firing_key    TEXT NOT NULL,
    kind          TEXT NOT NULL,
    target_name   TEXT NOT NULL,
    payload_json  BLOB NOT NULL CHECK (json_valid(payload_json)),
    created_at    TEXT NOT NULL,
    UNIQUE(job_id, attempt_id, policy_index, firing_key)
) STRICT;

CREATE TABLE webhook_deliveries (
    id               TEXT PRIMARY KEY,
    firing_id        TEXT NOT NULL REFERENCES health_firings(id),
    target_id        TEXT NOT NULL REFERENCES webhook_targets(id),
    state            TEXT NOT NULL,
    attempt_count    INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TEXT NOT NULL,
    response_code    INTEGER,
    last_error       TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    delivered_at     TEXT,
    UNIQUE(firing_id, target_id)
) STRICT;

CREATE INDEX webhook_deliveries_due
    ON webhook_deliveries(state, next_attempt_at, created_at);
