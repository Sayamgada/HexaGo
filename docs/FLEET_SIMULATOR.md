# Fleet Simulator & Live Stream (dispatch-go)

When `DEMO_MODE=true`, dispatch-go runs a fleet simulator next to the dispatch
broker and streams the fleet, rides and agent decisions over `/v1/stream`.
Code: `services/dispatch-go/internal/sim` (simulator), `internal/stream`
(WebSocket hub), `internal/store/batcher.go` (telemetry batching).

## Fleet

- `SIM_DRIVERS` drivers (default **300**, keep 300 on Render), IDs `d_001`…,
  spawned around `CITY_CENTER_LAT/LNG` (Bengaluru), 75% near the six demand hotspots
  (MG Road, Koramangala, Indiranagar, Whitefield, Electronic City, Kempegowda Airport).
- Exactly **15% autonomous vehicles** (`tier: "av"`, with `battery`); the rest are
  auto / mini / sedan / premium / xl. AVs drain 0.25%/km and recharge while parked.
- One tick per second. Idle drivers park for a while, then drive toward a
  hotspot (or an active demand spike), preferring nearby ones (weight × e^(−km/5)) so
  outlying areas keep their own supply. Assigned drivers drive to the pickup,
  then to the dropoff.

### Routing

Routes come from the public OSRM server (`OSRM_URL`, default
`https://router.project-osrm.org`):

- **Rate limited** to `OSRM_RPS` (default 1 req/s, the public server's policy) on
  a single worker; ride legs jump ahead of idle wandering.
- **Cached** in an LRU of 2,048 routes keyed by endpoints snapped to a ~220 m
  grid; wander targets are snapped to the same grid so drivers share routes.
- **Fallback**: a ride leg waits at most 3 s for OSRM, then drives a straight
  line. Idle drivers would rather stay parked than cut through buildings, so a
  slow wander route just delays the next move. `OSRM_URL=off` uses straight lines only.

### Ride lifecycle

The dispatcher notifies the simulator of every match; the simulator then
drives the ride through this state machine (`internal/sim/state.go`):

```
accepted --route_ready--> arriving --reached_pickup--> on_trip --reached_dropoff--> completed
```

| Sim status | Stored ride status | Driver status |
|---|---|---|
| `accepted` | `matched` | `en_route` |
| `arriving` | `matched` | `en_route` |
| `on_trip` | `in_progress` | `on_trip` |
| `completed` | `completed` | `idle` |

Transitions are persisted with `dispatch.Service.AdvanceRide`. A rider cancel
(`POST /v1/rides/{id}/cancel`) frees the driver on the next tick. Rides booked
through the public API are driven too when a simulated driver is matched.

## Demand

Requests per minute = base × demo time-of-day factor × scenario multipliers + spikes.

- **Base demand**: `SIM_DEMAND_PER_MIN`, default `auto`. Auto starts at drivers × 0.5 ÷
  12 min (12.5/min for 300 drivers) and then re-sizes itself every 30 s by Little's
  law: base = 0.5 × active drivers ÷ (measured busy minutes per ride × measured match
  share), frozen while a scenario or spike is active. This keeps roughly **40–60% of
  drivers busy** whether rides follow long OSRM road routes or straight lines (live
  OSRM rides take about twice as long; a fixed 12.5/min ran the fleet at 86% busy).
  `GET /v1/sim/config` shows `auto_demand`, `avg_ride_minutes` and `match_share`;
  setting `demand_per_min` there switches auto off.
- **Time of day** (IST): the real curve has commute peaks around 09:00 (×1.7) and
  18:00 (×1.8) and a quiet night (×0.2 at 03:00); the demo keeps 15% of that swing
  (×0.88–×1.12) so the fleet is never idle or saturated.
- **Random spikes**: about one every 10 minutes near a random hotspot, lasting 2–5 minutes.
- 85% of pickups are near a hotspot (σ ≈ 1 km), the rest anywhere in the city; trips
  are 1.5–7 km. Open requests are re-dispatched every 5 s and cancelled
  (`no_driver_timeout`) after 90 s.
- On start-up, non-terminal rides from a previous process are cancelled
  (`gateway_restart`) because their drivers no longer exist.

## Controls

| Endpoint | Body / effect |
|---|---|
| `GET /v1/sim/config` | Current state: target/live drivers, demand, active scenarios and spikes, OSRM cache stats, telemetry stats |
| `POST /v1/sim/config` | `{"drivers":int,"demand_per_min":float,"spike_lat":float,"spike_lng":float}`, all optional. `drivers` 0–2000 (busy drivers are removed after their trip); a spike adds 1.5× base demand (min 3/min) at that point for 10 min |
| `POST /v1/sim/scenario/rush_hour` | 25 requests across the hotspots at once, then demand ×3 and traffic ×0.75 for 5 min |
| `POST /v1/sim/scenario/rain_storm` | 25% of idle drivers go `offline` at once (back after 5 min), 12 requests at once, then demand ×2 and traffic 30% slower for 5 min |
| `POST /v1/sim/scenario/concert_exit` | 40 requests at Palace Grounds at once, then +45/min there for 5 min |
| `POST /v1/sim/scenario/safety_incident` | the on-trip vehicle with the longest remaining trip swerves 450 m off its route at 50 km/h, brakes hard to a standstill, waits 25 s, then resumes. It streams with `status:"incident"` meanwhile, and the Safety Sentinel's geofence rule emits a `safety_sentinel` agent_event (`used_fallback: true`) that is also written to `agent_events`. `409` if no vehicle is on a trip |

Unknown scenarios return `404`. With `DEMO_MODE=false` all `/v1/sim/*` routes return `503`.
Scenario start events appear on `ops` as agent `fleet_simulator`.

## WebSocket `/v1/stream`

Client → server:

```json
{"action":"subscribe","topics":["drivers","ops","ride:<id>"]}
{"action":"unsubscribe","topics":["ride:<id>"]}
```

Server → client, one JSON object per frame, **batched every 1 s**:

```json
{"topic":"drivers","data":[{"id":"d_123","lat":12.97,"lng":77.59,"heading":90.0,"speed_kmh":32.5,"status":"idle","tier":"sedan","battery":null}]}
{"topic":"ride:<id>","data":{"ride_id":"...","status":"arriving","driver":{"id":"d_123","...":"same driver object"},"eta_sec":240}}
{"topic":"ops","data":{"type":"agent_event","agent":"dispatch_broker","decision":"...","reasoning":"...","confidence":0.92,"latency_ms":3.1,"used_fallback":false,"ts":"RFC3339"}}
{"topic":"ops","data":{"type":"metrics","p50_ms":0.0,"p95_ms":0.0,"p99_ms":0.0,"matches_total":0,"active_drivers":0,"idle_drivers":0,"open_requests":0,"rides_in_progress":0}}
```

- Driver `status` is `idle`, `en_route`, `on_trip`, `offline` or `incident`. `battery` is `null` except for AVs.
- `drivers` and `ride:<id>` are snapshots: only the newest message per second is
  sent, and a new subscriber gets the latest snapshot on the next flush.
  `ops` keeps every message (up to 256 per second).
- Every dispatch decision (match or no match) publishes a `dispatch_broker`
  agent_event and writes a row to `agent_events`.
  agent_event may also carry `kind` (`match`, `no_match`, `incident`, `scenario`),
  `ride_id`, `driver_id`, `tier`, `requested_tier` and `upgraded` (omitted when empty).
  `latency_ms` and the metrics quantiles keep 0.1 µs resolution (e.g. `0.0424`).
- `metrics` has the same shape as `GET /v1/metrics/dispatch` and is pushed every
  second while the instance has `ops` subscribers.
- **Multi-instance**: with `REDIS_URL` the hub publishes through Redis pub/sub
  channel `hexago:stream`, so a client on any instance sees every instance's
  messages. Without Redis, fan-out is in-process.
- **Origins**: browsers must send an `Origin` on `CORS_ALLOWED_ORIGINS` (by default
  `https://hexago-web.onrender.com` plus `http://localhost:*` / `http://127.0.0.1:*`);
  others get `403`. Native clients without `Origin` are allowed.
- The server pings every 30 s; idle connections without pongs close after 75 s.
  Slow clients skip a window instead of blocking others.

Probe tool:

```bash
go run ./cmd/streamprobe -url wss://dispatch-go.onrender.com/v1/stream -topics drivers,ops -n 12 -origin https://hexago-web.onrender.com
```

## Telemetry & resource use

- Positions go to a `TelemetryBatcher` (also used for REST driver pings). It keeps
  the newest point per driver, skips drivers that have not moved, and writes one
  `COPY` every `TELEMETRY_FLUSH_INTERVAL` (default 5 s). Points older than
  `TELEMETRY_RETENTION` (default 2 h, `0` keeps everything) are pruned every 10 min
  so a small Postgres doesn't fill up.
- Simulated moves update the in-memory index without per-driver Redis calls. The
  Redis GEO mirror is synced in one pipelined `GEOADD` every 5 s.
- Tick duration goes to the Prometheus histogram `sim_tick_duration_seconds`, a
  summary is logged every minute (avg/max tick, fleet by status, OSRM cache), and
  any tick over 200 ms logs a warning. Locally, 300 drivers take about 0.4 ms per tick.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `DEMO_MODE` | `true` | Start the simulator |
| `SIM_DRIVERS` | `300` | Fleet size (0–2000) |
| `SIM_DEMAND_PER_MIN` | `auto` | Base requests per minute; `auto` (or 0) sizes it to the fleet for ~50% busy |
| `OSRM_URL` | `https://router.project-osrm.org` | OSRM base URL; `off` = straight lines |
| `OSRM_RPS` | `1` | OSRM request rate limit |
| `TELEMETRY_FLUSH_INTERVAL` | `5s` | Telemetry batch interval |
| `TELEMETRY_RETENTION` | `2h` | Telemetry retention (`0` = forever) |
