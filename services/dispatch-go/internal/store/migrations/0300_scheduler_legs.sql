-- Scheduled legs of confirmed multi-leg booking plans (internal/scheduler).
-- Each leg is dispatched through the normal matching flow at its pickup time
-- (in DEMO_MODE: 20 s after confirm, then 60 s after the previous leg completes).
-- `data` holds the whole leg (places, times, fare, ride/driver ids, notes).
CREATE TABLE IF NOT EXISTS scheduled_legs (
    id          TEXT PRIMARY KEY,
    plan_id     TEXT NOT NULL,
    rider_id    TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    status      TEXT NOT NULL,
    ride_id     TEXT,
    pickup_at   TIMESTAMPTZ NOT NULL,
    data        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS scheduled_legs_rider_idx ON scheduled_legs (rider_id, created_at DESC);
CREATE INDEX IF NOT EXISTS scheduled_legs_open_idx ON scheduled_legs (status) WHERE status IN ('scheduled', 'dispatched');
