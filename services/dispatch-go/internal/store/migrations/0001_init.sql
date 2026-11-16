-- Dispatch broker core schema.

CREATE TABLE IF NOT EXISTS riders (
    id          TEXT PRIMARY KEY,
    name        TEXT,
    phone       TEXT,
    rating      NUMERIC(3, 2) NOT NULL DEFAULT 5.00,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS drivers (
    id          TEXT PRIMARY KEY,
    name        TEXT,
    status      TEXT NOT NULL DEFAULT 'offline'
                CHECK (status IN ('offline', 'idle', 'en_route', 'on_trip')),
    rating      NUMERIC(3, 2) NOT NULL DEFAULT 0 CHECK (rating BETWEEN 0 AND 5),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS vehicles (
    id             TEXT PRIMARY KEY,
    driver_id      TEXT NOT NULL UNIQUE REFERENCES drivers (id) ON DELETE CASCADE,
    tier           TEXT NOT NULL CHECK (tier IN ('auto', 'mini', 'sedan', 'premium', 'xl', 'av')),
    is_autonomous  BOOLEAN NOT NULL DEFAULT false,
    battery_pct    DOUBLE PRECISION CHECK (battery_pct BETWEEN 0 AND 100),
    plate          TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rides (
    id             TEXT PRIMARY KEY,
    rider_id       TEXT NOT NULL REFERENCES riders (id),
    driver_id      TEXT REFERENCES drivers (id),
    tier           TEXT NOT NULL,
    status         TEXT NOT NULL
                   CHECK (status IN ('requested', 'matched', 'in_progress', 'completed', 'cancelled')),
    pickup_lat     DOUBLE PRECISION NOT NULL,
    pickup_lng     DOUBLE PRECISION NOT NULL,
    dropoff_lat    DOUBLE PRECISION NOT NULL,
    dropoff_lng    DOUBLE PRECISION NOT NULL,
    pickup_h3      TEXT,
    eta_seconds    DOUBLE PRECISION,
    distance_km    DOUBLE PRECISION,
    match_score    DOUBLE PRECISION,
    cancel_reason  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS rides_status_idx ON rides (status);
CREATE INDEX IF NOT EXISTS rides_driver_active_idx ON rides (driver_id)
    WHERE status IN ('matched', 'in_progress');

CREATE TABLE IF NOT EXISTS ride_legs (
    ride_id      TEXT NOT NULL REFERENCES rides (id) ON DELETE CASCADE,
    seq          INT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('pickup', 'trip')),
    from_lat     DOUBLE PRECISION NOT NULL,
    from_lng     DOUBLE PRECISION NOT NULL,
    to_lat       DOUBLE PRECISION NOT NULL,
    to_lng       DOUBLE PRECISION NOT NULL,
    distance_km  DOUBLE PRECISION NOT NULL,
    eta_seconds  DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (ride_id, seq)
);

-- High-volume location stream. No foreign keys, so it maps 1:1 onto a
-- ScyllaDB table partitioned by driver_id and clustered by recorded_at.
CREATE TABLE IF NOT EXISTS telemetry_points (
    id           BIGSERIAL PRIMARY KEY,
    driver_id    TEXT NOT NULL,
    lat          DOUBLE PRECISION NOT NULL,
    lng          DOUBLE PRECISION NOT NULL,
    heading      DOUBLE PRECISION NOT NULL DEFAULT 0,
    speed_kmh    DOUBLE PRECISION NOT NULL DEFAULT 0,
    status       TEXT,
    h3_cell      TEXT,
    recorded_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS telemetry_driver_time_idx ON telemetry_points (driver_id, recorded_at DESC);

-- Immutable audit trail of every agent decision.
CREATE TABLE IF NOT EXISTS agent_events (
    id             BIGSERIAL PRIMARY KEY,
    agent          TEXT NOT NULL,
    event_type     TEXT NOT NULL,
    ride_id        TEXT,
    input          JSONB NOT NULL DEFAULT '{}'::jsonb,
    decision       JSONB NOT NULL DEFAULT '{}'::jsonb,
    confidence     DOUBLE PRECISION,
    fallback_used  BOOLEAN NOT NULL DEFAULT false,
    token_cost     DOUBLE PRECISION NOT NULL DEFAULT 0,
    latency_ms     DOUBLE PRECISION,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_events_created_idx ON agent_events (created_at DESC);
CREATE INDEX IF NOT EXISTS agent_events_agent_idx ON agent_events (agent, created_at DESC);

CREATE OR REPLACE FUNCTION agent_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'agent_events is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS agent_events_no_mutation ON agent_events;
CREATE TRIGGER agent_events_no_mutation
    BEFORE UPDATE OR DELETE ON agent_events
    FOR EACH ROW EXECUTE FUNCTION agent_events_append_only();
