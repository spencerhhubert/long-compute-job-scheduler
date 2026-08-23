-- Worker event sequences are ordered within one agent session, not across a
-- worker's whole lifetime. An agent restarted with fresh local state begins a
-- new session and restarts its sequence numbers; that must not conflict with
-- history recorded under earlier sessions of the same worker.

CREATE TABLE worker_events_new (
    worker_id    TEXT NOT NULL REFERENCES workers(id),
    session_id   TEXT NOT NULL DEFAULT '',
    sequence     INTEGER NOT NULL,
    event_id     TEXT NOT NULL UNIQUE,
    attempt_id   TEXT NOT NULL REFERENCES attempts(id),
    kind         TEXT NOT NULL,
    payload_json BLOB NOT NULL CHECK (json_valid(payload_json)),
    occurred_at  TEXT NOT NULL,
    recorded_at  TEXT NOT NULL,
    PRIMARY KEY(worker_id, session_id, sequence)
) STRICT;

INSERT INTO worker_events_new(worker_id, session_id, sequence, event_id, attempt_id, kind, payload_json, occurred_at, recorded_at)
SELECT worker_id, '', sequence, event_id, attempt_id, kind, payload_json, occurred_at, recorded_at
FROM worker_events;

DROP TABLE worker_events;
ALTER TABLE worker_events_new RENAME TO worker_events;
