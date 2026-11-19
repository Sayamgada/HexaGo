# HexaGo Public API — quickstart

One gateway serves everything: **https://dispatch-go.onrender.com**

| | |
|---|---|
| Interactive reference (Swagger UI) | https://dispatch-go.onrender.com/docs |
| OpenAPI 3.1 document | https://dispatch-go.onrender.com/openapi.json (copy in [`docs/openapi.json`](openapi.json)) |
| SDKs | [`sdk/python/hexago.py`](../sdk/python/hexago.py), [`sdk/typescript/hexago.ts`](../sdk/typescript/hexago.ts) |

The free Render instances sleep when idle: the first request after a quiet spell can take up to a minute.

## 1. Get an API key

```bash
curl -s -X POST https://dispatch-go.onrender.com/v1/developer/keys \
  -H 'Content-Type: application/json' \
  -d '{"name":"my app","scopes":["rides:read","rides:write","stream:read","pricing:read"]}'
```

```json
{"id":"key_…","name":"my app","prefix":"hxg_3f9a1c","scopes":["rides:read","rides:write","stream:read","pricing:read"],
 "rate_limit_per_min":60,"api_key":"hxg_…","warning":"Store this key now: it is shown only once …"}
```

- The key is shown **once**; HexaGo stores only its SHA-256.
- Omit `scopes` to get every scope. `rate_limit_per_min` is 1–600 (default 60).
- Self-serve creation is limited to 5 keys per minute per IP.

Send the key as `Authorization: Bearer hxg_…` (or `X-API-Key: hxg_…`; WebSockets: `?api_key=hxg_…`).

```bash
export HEXAGO_KEY=hxg_…
```

### Scopes

| Scope | Grants |
|---|---|
| `rides:read` | rides, scheduled legs, ETAs, cell supply, dispatch metrics, safety incidents and check-ins, simulator status |
| `rides:write` | request / cancel rides, driver status, scheduling, safety check-in replies, wallet top-ups (demo) and refunds |
| `pricing:read` | `/v1/pricing/*` (quotes, rate card, surge) |
| `stream:read` | `/v1/stream` and `/v1/ws` WebSockets |
| `agents:invoke` | `/v1/agents/*` (booking agent, roster, events, LLM budget, settlement) and safety / simulator controls |
| `ledger:read` | wallets, journal entries, payouts, trial balance, ledger summary |

Each operation in the OpenAPI document names its scope in `x-required-scope`. A key without it gets `403` with `required_scope`.

### Rate limits

Each key has a token bucket (60 requests/minute by default, shared by every gateway instance through Redis). Every keyed response carries:

| Header | Meaning |
|---|---|
| `X-RateLimit-Limit` | requests per minute for this key |
| `X-RateLimit-Remaining` | requests left right now |
| `X-RateLimit-Reset` | seconds until the bucket is full again |
| `Retry-After` | on `429` only: seconds until the next request is allowed |

Requests **without** a key are the first-party path (the HexaGo web, desktop and Android apps and internal services). A browser request without a key from an origin outside the CORS allow-list is answered `401`.

## 2. curl examples

```bash
B=https://dispatch-go.onrender.com
H="Authorization: Bearer $HEXAGO_KEY"

# Request a ride (201 = matched, 202 = waiting for a driver)
curl -s -X POST $B/v1/rides -H "$H" -H 'Content-Type: application/json' \
  -d '{"rider_id":"rider_demo","pickup":{"lat":12.9716,"lng":77.5946},"dropoff":{"lat":12.9352,"lng":77.6245},"tier":"mini"}'

# Look it up
curl -s $B/v1/rides/<ride_id> -H "$H"

# Fare quote
curl -s -X POST $B/v1/pricing/quote -H "$H" -H 'Content-Type: application/json' \
  -d '{"tier":"sedan","pickup":{"lat":12.9716,"lng":77.5946},"dropoff":{"lat":13.1989,"lng":77.7068}}'

# Natural-language booking (Booking Intent agent; needs agents:invoke)
curl -s -X POST $B/v1/agents/booking/parse -H "$H" -H 'Content-Type: application/json' \
  -d '{"rider_id":"rider_priya","text":"take me to the airport at 6pm"}'

# The agent swarm and the ledger
curl -s $B/v1/agents -H "$H"
curl -s $B/v1/ledger/summary -H "$H"

# Watch the rate-limit headers
curl -s -D - -o /dev/null $B/v1/metrics/dispatch -H "$H" | grep -i x-ratelimit
```

## 3. Live stream (WebSocket)

`wss://dispatch-go.onrender.com/v1/stream?api_key=hxg_…` (scope `stream:read`). After connecting, subscribe:

```json
{"action":"subscribe","topics":["ride:<ride_id>"]}
```

Frames arrive about once a second as `{"topic","data"}`:

| Topic | `data` |
|---|---|
| `ride:<id>` | `{"ride_id","status","driver":{"id","lat","lng","heading","speed_kmh","status","tier","battery"},"eta_sec"}`; status `accepted → arriving → on_trip → completed` |
| `drivers` | every vehicle's position |
| `ops` | agent decisions (`{"type":"agent_event",…}`) and dispatch metrics |

With [websocat](https://github.com/vi/websocat):

```bash
websocat "wss://dispatch-go.onrender.com/v1/stream?api_key=$HEXAGO_KEY" <<< '{"action":"subscribe","topics":["ops"]}'
```

In a browser or Node 22+:

```js
const ws = new WebSocket(`wss://dispatch-go.onrender.com/v1/stream?api_key=${key}`);
ws.onopen = () => ws.send(JSON.stringify({ action: "subscribe", topics: [`ride:${rideId}`] }));
ws.onmessage = (e) => { const { data } = JSON.parse(e.data); console.log(data.status, data.driver.lat, data.driver.lng); };
```

## 4. SDKs: book a ride and stream its location

Python (`pip install websockets` for streaming; REST needs only the standard library) — [`sdk/python/example.py`](../sdk/python/example.py):

```python
hx = HexaGo.create_key("example")  # or HexaGo(api_key="hxg_...")
ride = hx.request_ride("rider_demo", pickup=(12.9716, 77.5946), dropoff=(12.9352, 77.6245))["ride"]
async for frame in hx.stream(f"ride:{ride['id']}"):
    d = frame["data"]
    print(d["status"], d["driver"]["lat"], d["driver"]["lng"], f"eta {d['eta_sec']}s")
```

TypeScript (browsers, Node 22+; no dependencies) — [`sdk/typescript/example.ts`](../sdk/typescript/example.ts), run with `node example.ts`:

```ts
const hx = await HexaGo.createKey("example");
const { ride } = await hx.requestRide("rider_demo", [12.9716, 77.5946], [12.9352, 77.6245]);
for await (const { data: d } of hx.stream<RideUpdate>(`ride:${ride.id}`)) console.log(d.status, d.driver.lat, d.driver.lng);
```

Both read `HEXAGO_BASE_URL` (default: the production gateway) so they can point at a local `go run ./cmd/server`.

## 5. Webhooks

```bash
curl -s -X POST $B/v1/developer/webhooks -H "$H" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/hexago","events":["ride.completed","incident.opened","payout.sent"]}'
# -> {"id":"wh_…","secret":"whsec_…",…}   (the secret is shown once)

curl -s -X POST $B/v1/developer/webhooks/<id>/test -H "$H"        # send a "ping" event
curl -s $B/v1/developer/webhooks/<id>/deliveries -H "$H"          # recent deliveries and attempts
```

| Event | `data` |
|---|---|
| `ride.completed` | the ride |
| `incident.opened` | `{incident_id, action (check_in / escalate), severity, reason, ride_id, driver_id, …}` |
| `payout.sent` | the payout (`amount_paise`, `owner`, `ride_id`, `status: "sent"`, …) |

Each delivery is a `POST` with body `{"id","type","created_at","data"}` and headers:

| Header | |
|---|---|
| `X-HexaGo-Signature` | `sha256=` + hex HMAC-SHA256(secret, `X-HexaGo-Timestamp` + `.` + raw body) |
| `X-HexaGo-Timestamp` | unix seconds (reject old ones to stop replays) |
| `X-HexaGo-Event` | event type |
| `X-HexaGo-Delivery` | event id, the same on every retry (use it to deduplicate) |

Answer with any 2xx. Anything else (or no answer within 5 s) is retried **3 times** with exponential backoff (1 s, 2 s, 4 s); a 4xx other than 408/429 is not retried. Targets must be public `http(s)` URLs (private, loopback and link-local addresses are refused); at most 10 webhooks per key.

Verify a signature (Python SDK: `verify_webhook(secret, body, ts, sig)`; TypeScript SDK: `verifyWebhook(...)`):

```python
import hashlib, hmac
expected = "sha256=" + hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(expected, request.headers["X-HexaGo-Signature"])
```

## 6. Key management

```bash
curl -s $B/v1/developer/keys/me -H "$H"            # the key's id, prefix, scopes and limit
curl -s -X DELETE $B/v1/developer/keys/me -H "$H"  # revoke it (its webhooks are removed too)
curl -s $B/v1/developer/scopes                     # scopes, events and the signature scheme
```

## Errors

dispatch-go answers errors as `{"error": "…"}`; endpoints served by agents-py (FastAPI) use `{"detail": …}`. Money is integer paise on the ledger (₹1 = 100 paise) and rupees on quotes.
