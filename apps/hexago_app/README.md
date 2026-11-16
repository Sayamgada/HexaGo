# hexago_app

Flutter client (web, desktop, Android) for HexaGo: Rider, Driver and the Ops Command Center.

## Ops Command Center (`/ops`)

The "zero humans in the loop" screen behind the Ops role card: a live Bengaluru fleet map,
dispatch KPIs, a streaming agent-decision feed and what-if scenario buttons.

Stack: Riverpod, go_router, dio, web_socket_channel, flutter_map.

### Build flags (`--dart-define`)

| Key | Default | Purpose |
|---|---|---|
| `API_BASE_URL` | `http://localhost:8080` | dispatch-go REST base |
| `WS_BASE_URL` | `ws://localhost:8080` | dispatch-go WebSocket base |
| `USE_MOCK` | `true` in debug, `false` in release | `true` = `MockOpsSource` (300 simulated cars), `false` = `LiveOpsSource` |
| `CARTO_API_KEY` | empty | CARTO now requires a key on every basemap tile (free tier: https://carto.com/basemaps/apikey). Without it the map draws a plain dark hex grid instead of tiles |

```powershell
flutter run -d chrome --dart-define=USE_MOCK=true --dart-define=CARTO_API_KEY=<key>
```

### Live backend contract (`LiveOpsSource`)

- `GET {API_BASE_URL}/v1/metrics/dispatch` returns `p50_ms, p95_ms, p99_ms, matches_total, active_drivers, idle_drivers, open_requests, rides_in_progress`
- `POST {API_BASE_URL}/v1/sim/scenario/{rush_hour|rain_storm|concert_exit|safety_incident}`
- WebSocket `{WS_BASE_URL}/v1/stream`: sends `{"action":"subscribe","topics":["drivers","ops"]}`, then receives
  `drivers` snapshots, `ops` `agent_event` frames and `ops` `metrics` frames.
  Reconnects with exponential backoff (1 s doubling, capped at 30 s). Until the first connection
  succeeds (for example while Render cold-starts), the map shows "Waking up the fleet...".

### Performance notes

All cars are drawn by a single `CustomPainter` layer (no per-marker widgets). A ticker runs only
while cars are moving between 1 s updates, and it repaints without rebuilding any widgets. Agent events are
batched every 250 ms so a burst of events causes one rebuild.

### Layout

- `lib/features/ops/models`: wire models and `parseOpsMessage`
- `lib/features/ops/data`: `OpsSource` interface, `MockOpsSource`, `LiveOpsSource`
- `lib/features/ops/state`: Riverpod providers, `FleetTracker` (interpolation)
- `lib/features/ops/widgets`: map, metrics strip, agent feed, banner, scenario bar
