-- Autonomous Incident Response (Safety Sentinel, agents-py).
-- Kept identical to services/agents-py/src/agents_py/agents/safety/store.py DDL,
-- which creates the same tables when agents-py starts before dispatch-go migrates.
-- incidents: one row per check-in or escalation (status check_in | escalated | monitoring | resolved);
--   triggers, actions, transcript, planned route and trail live in `data`.
-- incident_timeline: every step (timestamp, actor agent, details).
-- safety_checkins: the rider call sessions.
CREATE TABLE IF NOT EXISTS incidents (
    id            TEXT PRIMARY KEY,
    ride_id       TEXT,
    driver_id     TEXT,
    rider_id      TEXT,
    tier          TEXT,
    signal_id     TEXT,
    severity      DOUBLE PRECISION NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    decision      TEXT NOT NULL,
    reason        TEXT,
    reasoning     TEXT,
    data          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    resolved_by   TEXT,
    resolution    TEXT
);
CREATE INDEX IF NOT EXISTS incidents_created_idx ON incidents (created_at DESC);
CREATE INDEX IF NOT EXISTS incidents_driver_idx ON incidents (driver_id, created_at DESC);
CREATE TABLE IF NOT EXISTS incident_timeline (
    id           TEXT PRIMARY KEY,
    incident_id  TEXT NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    step         TEXT NOT NULL,
    actor        TEXT NOT NULL,
    details      JSONB NOT NULL DEFAULT '{}'::jsonb,
    at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS incident_timeline_incident_idx ON incident_timeline (incident_id, at);
CREATE TABLE IF NOT EXISTS safety_checkins (
    id           TEXT PRIMARY KEY,
    incident_id  TEXT NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    ride_id      TEXT,
    question     TEXT NOT NULL,
    status       TEXT NOT NULL,
    timeout_s    DOUBLE PRECISION NOT NULL,
    data         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline_at  TIMESTAMPTZ NOT NULL,
    answered_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS safety_checkins_incident_idx ON safety_checkins (incident_id);
