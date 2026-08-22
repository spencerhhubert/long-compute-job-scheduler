CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    token_hash   BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    scope        TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
) STRICT;

CREATE TABLE browser_sessions (
    session_hash BLOB PRIMARY KEY CHECK (length(session_hash) = 32),
    api_token_id TEXT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL
) STRICT;

CREATE INDEX browser_sessions_expiry
    ON browser_sessions(expires_at);
