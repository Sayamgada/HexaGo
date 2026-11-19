-- Demand Forecast (agents-py agents/forecast) reads ride requests by creation
-- time every 60 s: the last 2 hours plus +-30 min on each of the previous 7 days.
CREATE INDEX IF NOT EXISTS rides_created_at_idx ON rides (created_at);
