# PROJECT_CONTEXT: HexaGo

## 1. Product Vision
**HexaGo** is an AI-native Mobility-as-a-Service (MaaS) ride-hailing platform (an Uber replica) engineered for **zero-human operations**.
- **AI-Native Operations**: Every operational task—from booking negotiation, matching, dynamic surge pricing, pooling optimization, and real-time safety monitoring to fraud settlement and jurisdiction compliance—is orchestrated by autonomous, collaborative AI agents.
- **100% AI-Generated Codebase**: All software architecture, services, application layers, infrastructure blueprints, and automated tests are authored and maintained by AI agents. No hand-written application code.

---

## 2. System Architecture

```mermaid
flowchart TD
    Client["Flutter Client (Web / Desktop / Android)"]
    Gateway["dispatch-go (Go Gateway: Chi + WebSockets + H3)"]
    Agents["agents-py (Python FastAPI + LangGraph + LiteLLM)"]
    DB[("PostgreSQL + pgvector")]
    Redis[("Redis (Live Locations & Pub/Sub)")]
    LLM["LLM Providers (via LiteLLM / Gemini)"]

    Client -->|REST & WebSocket /v1/*| Gateway
    Gateway -->|Reverse Proxy /v1/agents/*| Agents
    Gateway -->|Driver Coordinates & Geofencing| Redis
    Gateway -->|Trips & Persistence| DB
    Agents -->|Embeddings & Knowledge / Agent Logs| DB
    Agents -->|Agent Invocation & Fallbacks| LLM
    Agents -->|State Coordination & Pub/Sub| Redis
```

### Components
1. **Frontend (`apps/hexago_app`)**:
   - Built with Flutter supporting Web, Desktop (Windows, macOS, Linux), and Android.
   - Unified interface offering three operational personas:
     - **Rider**: Real-time quote negotiation, trip tracking, pool acceptance, safety status.
     - **Driver**: Telemetry ingestion, dispatch offers, dynamic route recommendations, settlement ledger.
     - **Ops Command Center**: Real-time agent event stream, swarm health metrics, override controls, override simulation.

2. **API Gateway & Core Dispatch (`services/dispatch-go`)**:
   - Written in Go 1.22+ using Chi router, Gorilla WebSocket, Uber H3 spatial indexing (`uber/h3-go/v4`), `pgx`, and `go-redis`.
   - Acts as the single public API gateway exposing REST endpoints and persistent bi-directional WebSockets.
   - Exposes `/healthz`, `/metrics` (Prometheus), and `/v1/ping`.
   - Reverse-proxies `/v1/agents/*` directly to `agents-py`.
   - Manages high-throughput driver location pings, H3 hexagon quantization, and spatial indexing in Redis.
   - Take-over mode (`internal/sim/takeover.go`, routes in `internal/server/routes_takeover.go`, `TAKEOVER_ENABLED`, default true): opening a driver in the Driver module calls `POST /v1/drivers/{id}/takeover` (TTL 10 min, renewed every minute while the screen is open; `DELETE` or expiry reverts to the simulator). A human-controlled car parks instead of wandering and stays online in a rain storm; rides matched to it are offered instead of auto-accepted: a `ride_offer` (pickup, drop, fare, ETA to pickup, 15 s timeout) goes out on stream topic `driver:<id>` and `POST /v1/rides/{id}/offer/{accept|decline}` answers it. Accept runs the normal ride flow; decline, timeout or release re-run matching without that driver (`dispatch.RematchExcluding`; a declined ride is never offered to the same driver again) and close the offer with `offer_closed`. `POST /v1/drivers/{id}/send-ride` (DEMO_MODE) books a ride ~0.4-0.8 km from the driver and assigns it to that driver (`dispatch.CreateRideForDriver`). `GET /v1/route` returns OSRM road geometry (straight line as fallback) for the Driver module's "Move to hotspot" card; `/v1/forecast/*` is proxied to agents-py.
   - Clearing ledger (`internal/ledger`, migration `0200_ledger_core.sql`, tables `accounts`, `journal_entries`, `postings`, `payouts`; in-memory `Store` without `DATABASE_URL`): double-entry, int64 paise, every entry sums to zero (also a deferred Postgres trigger), entries are append-only (refunds are reversal entries), wallets and payables can never go negative (CHECK constraint). A completed ride (`dispatch.OnRideCompleted`) is settled by the pure `ComputeSplit` (driver 75 % or fleet/AV operator 80 %, city levy 2 %, GST 5 % on the platform fee, promo from `promo_wallet`, tip 100 % to the payee, rounding remainder to `platform_revenue`) as one entry with idempotency key `ride:<id>`, then paid out (`driver_payable -> payment_clearing`) within 300 ms. A safety escalation (driver suspension) settles the interrupted ride with its payout `held_for_review`; the Ops Supervisor-confirmed unsuspend releases it. Endpoints: `GET /v1/wallets/{owner}`, `POST /v1/wallets/{owner}/topup` (DEMO_MODE), `GET /v1/ledger/entries?ride_id=`, `GET /v1/ledger/trial-balance`, `GET /v1/ledger/summary`, `GET /v1/ledger/payouts`, `POST /v1/ledger/refunds`. Events `ledger_entry`, `payout_sent`, `payout_held`, `payout_released`, `refund` go to the `ops` topic (agent `settlement`). The Settlement agent (agents-py `agents/settlement`) audits entries and payouts every `SETTLEMENT_SCAN_INTERVAL_S` (60 s) with deterministic checks; the LLM only explains flagged items, asynchronously.

3. **Autonomous Agent Swarm (`services/agents-py`)**:
   - Built with Python 3.12, FastAPI, LangGraph, LiteLLM, SQLAlchemy, pgvector, and pytest-cov.
   - Houses the multi-agent graph workflows governing dispatch, safety, pricing, and settlements.
   - Connects to PostgreSQL with `pgvector` for agent episodic memory and semantic lookup.
   - Agent platform core (`src/agents_py/core`): `llm.py` (LiteLLM wrapper with cost guardrails), `events.py` (`agent_events` audit rows + Ops feed frames), `memory.py` (pgvector recall with keyword fallback), `graph.py` (LangGraph helpers, `run_with_fallback`, `@agent` registration), `registry.py` (the 11-agent roster). `clients/dispatch.py` talks to dispatch-go.
   - Endpoints (reached through the gateway): `GET /v1/agents` (roster with status, `decisions_last_hour`, `last_decision`, `fallback_rate`), `GET /v1/agents/events?limit=&agent=`, `GET /v1/agents/llm/budget`.
   - Safety Sentinel (`agents/safety`): dispatch-go's `internal/safety` detector POSTs `safety_signal`s to `/v1/safety/signal`; the sentinel decides monitor | check_in | escalate with pure rules (LLM reasoning async, upgrade-only), calls the rider (`safety_call` on `ride:<id>` + `ops`, reply via `POST /v1/safety/checkins/{id}/reply`, 15 s in DEMO_MODE / 30 s), and escalates through dispatch-go (simulated emergency dispatch, `/track/{token}`, driver suspension, contact SMS). `GET /v1/safety/incidents[/{id}]`; closing needs `POST /v1/safety/incidents/{id}/resolve` confirmed by the Ops Supervisor. Tables `incidents`, `incident_timeline`, `safety_checkins` (migration `0100_safety_incidents.sql`).
   - Demand Forecast (`agents/forecast`, `FORECAST_ENABLED`, default true): every `FORECAST_INTERVAL_S` (60 s) it reads ride requests from Postgres `rides` (last 2 h, plus +-30 min around the same time on each of the previous 7 days; dispatch-go's 15-minute supply snapshot without Postgres), bins them per H3 res-8 cell into 5-minute buckets and forecasts demand now / +15 / +30 min with Holt smoothing (damped trend), a Bengaluru time-of-day curve, a 30 % same-hour-history blend and active scenario boosts (rain_storm: x1.4 in the centre to x2.0 6 km out; concert_exit: up to x3.5 within 2 km of the venue; rush_hour: x1.5; boosts fade after the scenario ends). Each cell carries `demand_score` (absolute, 12 rides/h = 0.5), `expected_earnings_per_hr` (INR 220 average fare x 75 % driver share x trips shared with the idle cars in the cell, max 2.5 trips/h) and a template explanation; the top-5 hotspots at +15 min get a one-line LLM explanation asynchronously, only when a top cell lacks one and 15 min passed or the scenarios changed. One `forecast_run` event per run. `GET /v1/forecast/heatmap?horizon=0|15|30`.
   - Ops feed: agent events are published to Redis channel `ops` (plain frame) and to `hexago:stream` as `ops
<frame>`, which dispatch-go relays to `/v1/stream` subscribers of the `ops` topic. The Ops Supervisor writes a deterministic heartbeat event every `HEARTBEAT_INTERVAL_S` (300 s).
   - With no `DATABASE_URL` / `REDIS_URL` (or if they fail), events live in a 500-entry in-memory ring and guardrail counters in process memory; the service never crashes on missing infra.
   - Legal & Jurisdiction (`agents/legal`, `LEGAL_ENABLED`, default true): tables `jurisdictions` (Bengaluru, Mumbai, Delhi seeded; rules `max_surge_multiplier`, `min_insurance_coverage` INR, `max_driving_hours`, `av_allowed_zones` H3 res-7 cells, `required_documents`, `city_levy_pct`) and `legal_audit`, created by agents-py. `POST /v1/legal/ingest {city?, text}`: the LLM extracts a change set (`complete_json`), the regex extractor is its fallback and its explicit numbers win over the LLM, values are bounds-checked, and the old -> new diff is applied at once with an audit row and a `legal_rule_change` agent event. `GET /v1/legal/jurisdictions[/{city}]`, `GET /v1/legal/audit`. Pricing reads the cap only through `get_surge_cap(city)` (30 s cache, invalidated on a local change) for the surge map and quotes of `CITY_NAME` (default `Bengaluru`); `city_levy_pct` is informational (the ledger levy stays 2 %).
   - Autonomous Ops Report (`agents/supervisor`, `OPS_REPORT_ENABLED`, default true, every `OPS_REPORT_INTERVAL_S` = 300 s and on demand): reads the roster and fallback rates from `agent_events`, `/v1/metrics/dispatch`, `/v1/ledger/summary`, incidents and the LLM budget; anomalies: fallback rate > 50 % over >= 5 decisions, a periodic agent active in the last hour but silent for 10 min, a `check_in`/`escalated` incident open > 15 min, trial balance != 0; auto-actions: re-run the silent agent, trigger a Settlement audit, alert the feed, flag incidents for review. Deterministic markdown report (KPIs, decisions per agent, incidents, money, anomalies and actions, "Human interventions: 0"); the LLM writes the executive summary asynchronously. Table `ops_reports`. `GET /v1/ops/report`, `GET /v1/ops/reports[/{id}]`, `POST /v1/ops/report/run`. dispatch-go proxies `/v1/legal/*` and `/v1/ops/*` (scope `agents:invoke`).

4. **Data & State Layer**:
   - **PostgreSQL + pgvector**: Relational storage for users, vehicles, trips, payments, and the append-only `agent_events` audit table.
   - **Redis**: In-memory ephemeral store for driver geospatial indexes (`GEOADD` / H3 sets), live session state, and inter-service Pub/Sub.

### Live Deployment (Render, Free plan, created manually)
| Service | URL |
|---|---|
| dispatch-go (public gateway) | https://dispatch-go.onrender.com |
| agents-py | https://hexago-65oz.onrender.com |
| hexago-web (Flutter Web) | https://hexago-web.onrender.com |

Redis (Render Key Value `hexago-redis`, Singapore) lives in a **different Render account** than the three services, so internal `redis://red-…` URLs cannot resolve. Both dispatch-go and agents-py use its **External URL** (`rediss://…@singapore-keyvalue.render.com:6379`, TLS) and the instance's Valkey inbound IP rules must allow them. `/healthz?redis=fresh` on either service runs a live connect / PING / pub-sub check and reports the masked URL, failing stage and a hint.

The web app calls only the gateway; `/v1/agents/*` is proxied to agents-py. Both services allow CORS from `CORS_ALLOWED_ORIGINS` (default: the web app origin plus `http://localhost:*` and `http://127.0.0.1:*` for local dev).

---

## 3. Autonomous Agent Roster

HexaGo operates with eleven specialized autonomous agents coordinating via LangGraph workflows:

| Agent Name | Primary Responsibility | Deterministic Fallback Mode |
|---|---|---|
| **Booking Intent** | Parses natural language ride requests, extracts origin/destination/preferences, disambiguates vague drop-offs. | Standard regex & structured form parser. |
| **Dispatch Broker** | Matches rider demand to nearest drivers using H3 spatial clustering and driver preference scores. | Classical nearest-neighbor greedy dispatch based on H3 distance. |
| **Pool Batching** | Analyzes overlapping trip trajectories within 90-second batch windows for multi-rider pooling. | First-come route detour threshold calculation (< 15% added ETA). |
| **Surge Equilibrium** | Analyzes real-time supply/demand elasticity and live traffic to balance pricing. | Fixed multiplier curve based on hex-cell supply/demand ratio. |
| **Demand Forecast** | Predicts hex-level ride requests 15–60 minutes ahead to preemptively position drivers. | Historical moving average by day-of-week and hour bucket. |
| **Safety Sentinel** | Continuously evaluates route anomalies, sudden stops, route deviations, and audio telemetry distress. | Geofence deviation alert (> 500m off planned path for > 3 min). |
| **Compliance Vision** | Audits vehicle inspection photos, driver documents, and regulatory roadworthiness. | Manual hold queue + heuristic OCR verification. |
| **Legal & Jurisdiction** | Adapts tariff regulations, municipal compliance rules, driver working-hour limits per jurisdiction. | Static jurisdiction lookup table by municipal boundaries. |
| **Settlement** | Computes driver earnings, platform take-rates, refunds, dynamic cancellation compensations. | Deterministic distance + duration rate formula. |
| **Support & Dispute** | Handles passenger complaints, lost-and-found items, and route dispute arbitration. | Standard canned escalation workflow & credit issue matrix. |
| **Ops Supervisor** | Monitors multi-agent health, detects reasoning drift, throttles rogue actions, and enforces platform guardrails. | Automatic agent circuit breaker to non-LLM rule engines. |

---

## 4. Coding Conventions & Standards

1. **Test-Driven AI Development**:
   - Tests **must** be written and verified before feature implementation.
   - CI executes test suites with coverage thresholds for every PR:
     - Go: `go test -race -cover ./...`
     - Python: `pytest --cov=. --cov-report=term-missing`
     - Flutter: `flutter analyze` and `flutter test --coverage`
2. **Observability & Diagnostics**:
   - Every service must expose:
     - `/healthz`: Liveness and readiness probe responding with HTTP 200 and structured component status.
     - `/metrics`: Prometheus-compatible metrics endpoint.
3. **Configuration & Secrets**:
   - Zero hardcoded secrets or environment targets. All runtime configuration is driven strictly through environment variables.
   - Standardized keys: `LLM_MODEL`, `GEMINI_API_KEY`, `DATABASE_URL`, `REDIS_URL`, `AGENTS_URL`, `DISPATCH_URL`, `DEMO_MODE`, `CITY_CENTER_LAT`, `CITY_CENTER_LNG`, `CORS_ALLOWED_ORIGINS`, `FORECAST_ENABLED`, `FORECAST_INTERVAL_S`, `TAKEOVER_ENABLED`.
   - LLM guardrail keys (agents-py): `LLM_ENABLED` (default `true`), `LLM_TIMEOUT_S` (`4`), `LLM_BACKOFF_S` (`0.5`), `LLM_DAILY_CALL_LIMIT` (`300`), `LLM_DAILY_TOKEN_LIMIT` (`200000`), `LLM_MIN_INTERVAL_S` (`20`), `LLM_CACHE_TTL_S` (`600`), `EMBEDDING_MODEL` (`gemini/gemini-embedding-001`, requested at 768 dims), `HEARTBEAT_INTERVAL_S` (`300`, `0` disables).
4. **Pluggable LLM Backends**:
   - All agent inference routes through LiteLLM configured by `LLM_MODEL` (default `gemini/gemini-3.5-flash-lite`; e.g. `openai/gpt-4o-mini`, `anthropic/claude-3-5-haiku`), then `LLM_FALLBACK_MODELS` (comma-separated, default `gemini/gemini-3.1-flash-lite,gemini/gemini-3-flash-preview`). A model that answers 404 is dropped for the life of the process; a 429/503 backs off (`LLM_BACKOFF_S`, doubling, max 8 s) and moves to the next model. gemini-2.x and `text-embedding-004` answer 404 as of 2026-11-18.
5. **Deterministic Non-LLM Fallbacks**:
   - Every single agent must implement a reliable, deterministic non-LLM fallback algorithm. If the LLM provider times out, throttles, or returns malformed structured output, the platform continues uninterrupted without human intervention.
6. **Immutable Audit Trail (`agent_events`)**:
   - Every agent invocation, input context, model decision, confidence score, token cost, and fallback activation is logged to the `agent_events` database table. The Ops Command Center visualizes this live stream.
7. **LLM Cost Guardrails (mandatory)**:
   - Agents never call a provider directly: every completion goes through `core.llm.complete_json(prompt, schema, agent_name, fallback)` and every embedding through `LLMClient.embed`.
   - Checks, in order: `LLM_ENABLED` (and a `GEMINI_API_KEY` for `gemini/*` models) → response cache (sha256 of model + prompt in Redis, 10 min TTL) → per-agent minimum interval `LLM_MIN_INTERVAL_S` → daily call and token budgets, kept in Redis and reset at midnight IST (in-memory fallback if Redis is down).
   - Provider calls time out after `LLM_TIMEOUT_S`; each failed attempt moves to the next model in the chain (a single-model chain is retried once). All guardrail time reads (day keys, min interval, cache TTL) go through the injected clock; Redis only ever gets relative TTLs. Whenever a guardrail blocks, the timeout hits, the provider errors, or the output fails schema validation, the caller's deterministic fallback runs and the result is marked `used_fallback=true` with `fallback_reason` one of `disabled | budget | rate | timeout | error`.
   - LLM calls are event-driven only: agents call the LLM on meaningful events (booking request, incident, unusual surge), never on every tick.
   - `GET /v1/agents/llm/budget` reports `calls_today`, `tokens_today`, `limits`, `enabled`, `cache_hits`.
8. **Default Demo City**:
   - **Bengaluru, India**
   - Configurable via `CITY_CENTER_LAT` (default: `12.9716`) and `CITY_CENTER_LNG` (default: `77.5946`).

