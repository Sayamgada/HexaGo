# Spatio-Temporal Dispatch Broker (dispatch-go)

Deterministic, non-LLM matcher that lives in the Go gateway. Code: `services/dispatch-go/internal/{geo,fleet,matching,metrics,store,dispatch}`.

## How a match works

1. The pickup is mapped to an H3 cell (resolution `H3_RESOLUTION`, default 9 ≈ 0.1 km² cells).
2. Rings around that cell are scanned one at a time (k = 0, 1, 2, 3). Expansion stops once 4 eligible drivers (best + 3 alternatives) are found, or after ring 3 (≈ 1 km at res 9).
3. Eligible means `status = idle`, tier matches, and for `av` the battery covers pickup + trip at 0.25 %/km plus a 15 % reserve.
4. Each candidate gets a score in [0, 1]:

| Component | Weight | Formula |
|---|---|---|
| ETA | 0.45 | `1 − eta / 15 min`; `eta = haversine × 1.3 road factor ÷ (max(speed, 30 km/h) × CITY_SPEED_FACTOR)` |
| Momentum | 0.15 | `(1 + cos(heading − bearing to pickup)) / 2`; 0.5 if moving < 3 km/h |
| Rating | 0.15 | `(rating − 1) / 4`; unrated = 4.5 |
| Idle time | 0.15 | `idle / 20 min`, capped at 1 (fairness) |
| Battery | 0.10 | AV: surplus over requirement ÷ headroom; non-AV: 1 |

5. The top driver is claimed atomically (`idle → en_route`). If another request took it first, the next ranked driver is tried.

The fleet index is sharded 64 ways by driver ID and 64 ways by H3 cell, with one lock per shard. When `REDIS_URL` is set, positions are mirrored to the Redis GEO set `fleet:drivers:geo`. If Redis is down, a single warning is logged and the in-memory index keeps working.

Without CGO (Windows dev machines), `geo` uses a pure-Go hex grid with H3's edge lengths instead of the H3 C library. CI, Docker and Render all build with CGO and use real H3.

## REST API

| Method & path | Body | Result |
|---|---|---|
| `POST /v1/drivers/{id}/status` | `{status, lat, lng, heading, speed_kmh, tier, battery_pct, rating}`: all optional; a new driver needs `lat`, `lng`, `tier` | `200 {driver}` |
| `POST /v1/rides` | `{rider_id, pickup:{lat,lng}, dropoff:{lat,lng}, tier}` | `201 {ride, match}` if matched; `202 {ride}` if stored as an open request |
| `GET /v1/rides/{id}` | | `200 {ride}` / `404` |
| `POST /v1/rides/{id}/cancel` | `{reason}` (optional) | `200 {ride}`; `409` once the ride is in progress or finished |
| `GET /v1/metrics/dispatch` | | `{"p50_ms","p95_ms","p99_ms","matches_total","active_drivers","idle_drivers","open_requests","rides_in_progress"}` |

`match` contains the chosen driver, its score breakdown, up to 3 `alternatives` with their breakdowns, `candidates_considered`, `rings_searched` and `latency_ms`.

Ride lifecycle: `requested → matched → in_progress → completed`, or `→ cancelled`. Driver status updates drive it:
- driver `on_trip` → ride `in_progress`
- driver leaves `on_trip` → ride `completed`
- a matched driver going `idle`/`offline` → the ride reopens as `requested`

`open_requests` counts `requested` rides. `rides_in_progress` counts `matched` + `in_progress` rides. Latency percentiles are taken over the last 4096 match attempts. Prometheus exposes `dispatch_match_latency_seconds` (histogram), `dispatch_matches_total{outcome}`, `dispatch_active_drivers` and `dispatch_idle_drivers` on `/metrics`.

## Persistence

On boot, embedded migrations (`internal/store/migrations`) create `riders`, `drivers`, `vehicles`, `rides`, `ride_legs`, `telemetry_points` and `agent_events`. A Postgres advisory lock serialises concurrent boots, and a trigger keeps `agent_events` append-only. Each match writes a `dispatch_broker` event (`match` / `no_match`), with confidence = total score.

Telemetry goes through the `TelemetryStore` interface (`PostgresTelemetry` today) so it can move to ScyllaDB later. `telemetry_points` has no foreign keys for that reason.

Without `DATABASE_URL`, or if Postgres can't be reached at boot, the service runs on an in-memory store and logs a warning. `/healthz` reports this under `components`.

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `H3_RESOLUTION` | `9` | Driver index resolution (0–15) |
| `CITY_SPEED_FACTOR` | `0.6` | Congestion multiplier on driving speed, in (0, 1] |
| `DATABASE_URL` | *(empty)* | Postgres; empty = in-memory |
| `REDIS_URL` | *(empty)* | Redis GEO mirror; empty = none |

## Integration tests

`TestPostgresContract` and `TestRedisMirrorIntegration` run only when `TEST_DATABASE_URL` / `TEST_REDIS_URL` are set. Each Postgres run uses a throwaway schema. On Windows a native Postgres may also hold port 5432, so run them in Docker on the compose network:

```bash
docker run --rm --network hexago_default -v "$PWD:/src" -w /src -e CGO_ENABLED=1 -e TEST_DATABASE_URL="postgres://hexago:<password>@postgres:5432/hexago?sslmode=disable" -e TEST_REDIS_URL=redis://redis:6379/0 golang:1.26-bookworm go test -race -cover ./...
```
