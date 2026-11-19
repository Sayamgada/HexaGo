-- Public API platform: API keys (SHA-256 of the key only, never the key) and webhooks.
CREATE TABLE IF NOT EXISTS api_keys (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    prefix             TEXT NOT NULL,
    key_hash           TEXT NOT NULL UNIQUE,
    scopes             TEXT[] NOT NULL,
    rate_limit_per_min INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_per_min BETWEEN 1 AND 600),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at         TIMESTAMPTZ
);

-- The signing secret is kept in clear: HMAC needs it to sign every delivery.
CREATE TABLE IF NOT EXISTS webhooks (
    id         TEXT PRIMARY KEY,
    key_id     TEXT NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    url        TEXT NOT NULL,
    events     TEXT[] NOT NULL,
    secret     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS webhooks_key_idx ON webhooks (key_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS webhooks_events_idx ON webhooks USING GIN (events) WHERE deleted_at IS NULL;
