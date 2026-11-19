# HexaGo: 10-minute demo script

Ordered by rubric weight. Each step has **clicks**, **say** (talk track), and **if it fails live** (the fallback).

## Before you start (T-15 min)

1. Wake all three free Render services and check everything once:
   ```bash
   services/agents-py/.venv/Scripts/python.exe tools/smoke.py
   ```
   Expect `15/15 passed`. It creates one ride, one safety incident (answered "I'm fine") and one API key, which it revokes.
2. Open these tabs:
   - Web app: https://hexago-web.onrender.com
   - Swagger UI: https://dispatch-go.onrender.com/docs
   - [docs/AUTOCODING_LOG.md](AUTOCODING_LOG.md) and [docs/AUTOCODING_STATS.md](AUTOCODING_STATS.md) on GitHub
   - Pull requests: [Sayamgada/HexaGo#1](https://github.com/Sayamgada/HexaGo/pull/1) and [Sayamgada/HexaGo#16](https://github.com/Sayamgada/HexaGo/pull/16)
   - Latest Actions run on `main` (the coverage-gate job summary)
3. Copy the golden sentence to the clipboard:
   > Pick up my kid from school at 3:15 PM, drop them at soccer practice, and then book a high-tier sedan for me from my office to the airport to arrive by 6:00 PM.
4. Backup: a local screen recording of the full flow, in case the network fails.

| Time | Step | Rubric area |
|---|---|---|
| 0:00 to 1:00 | 1. Ops Command Center | Zero-human operations |
| 1:00 to 3:00 | 2. Geospatial latency | Dispatch performance |
| 3:00 to 5:00 | 3. Autocoding | 100% AI-built |
| 5:00 to 7:00 | 4. Safety | Autonomous incident response |
| 7:00 to 8:30 | 5. Rider | Natural-language booking, pricing |
| 8:30 to 9:30 | 6. Finance | Real-time clearing ledger |
| 9:30 to 10:00 | 7. Public API and downloads | Platform |

---

## 1. Ops Command Center (1 min)

**Clicks**
1. Home: click the **Ops Command Center** card ("Agent Swarm Supervisor").
2. Point at the banner: **"0 humans in the loop · N autonomous decisions today"**.
3. Point at the live fleet map (Bengaluru, 300 simulated vehicles) and the metric strip (Active drivers, Idle, Open requests, Rides in progress, Match p50, Match p99, Matches today).
4. Right panel: click the **Agents** tab. Each card shows ACTIVE or PLANNED, its decisions in the last hour, its model or "Deterministic rules", and its last decision.

**Say**
"This is a live city: Bengaluru, 300 vehicles, simulated riders. Nobody is operating it. Every dispatch, price, safety call and payout is an agent decision, logged to an append-only `agent_events` table. Six of our eleven agents are live today; the other five are marked PLANNED, and we're open about that. Every agent has a deterministic fallback, so the city keeps running if the LLM times out or hits its budget."

**If it fails live:** if the screen says "Waking up the agent swarm...", keep talking; it recovers within a minute. If the map stays empty, show `GET https://dispatch-go.onrender.com/v1/metrics/dispatch` and `/v1/agents` in a browser tab: same numbers, raw.

## 2. Geospatial latency (2 min)

**Clicks**
1. Stay on Ops. Point at **Match p50** and **Match p99** in the metric strip. Typical live values: p50 about 120 µs, p99 about 600 µs.
2. In the scenario bar, click **Rush hour**. Watch Open requests rise and Matches today climb while p99 stays sub-millisecond.
3. Click **Agent feed** and expand a Dispatch Broker match event: it shows the rings searched, search radius, candidates considered, score breakdown (ETA 0.45, momentum 0.15, rating 0.15, idle-time fairness 0.15, battery 0.10), and alternatives.

**Say**
"Matching is pure Go, with no LLM in the hot path. The pickup becomes an H3 cell at resolution 9. We scan k-rings outward and stop as soon as we have five eligible drivers, so a dense area resolves in two or three rings, and a sparse one widens adaptively up to the 5 km cap, which is 18 rings. If your tier is empty we offer a free upgrade before we give up. The fleet index is sharded 64 ways by driver and 64 ways by cell. p99 is in microseconds, measured in production, not in a benchmark. Every match explains itself in the feed."

**If it fails live:** open `https://dispatch-go.onrender.com/v1/metrics/dispatch` (`p50_ms` and `p99_ms` are milliseconds with 0.1 µs resolution) and [docs/DISPATCH_BROKER.md](DISPATCH_BROKER.md) for the algorithm. If the cold-start ramp makes p99 look high, say so: the first minute after a restart includes JIT and cache warm-up.

## 3. Autocoding (2 min)

**Clicks**
1. GitHub: open [docs/AUTOCODING_LOG.md](AUTOCODING_LOG.md). Scroll the table: every session, its prompt, its files and tests, all by Claude Code or Antigravity.
2. Open the latest Actions run on `main`: show the jobs `go` (race detector, real H3), `python`, `flutter`, `tools` and `coverage-gate`. Click into the coverage-gate job summary: the per-package table, with the gate at 96%.
3. Open [Sayamgada/HexaGo#1](https://github.com/Sayamgada/HexaGo/pull/1): a deliberate off-by-one in the hex k-ring walk. CI went red.
4. Open [Sayamgada/HexaGo#16](https://github.com/Sayamgada/HexaGo/pull/16): the self-heal workflow read the failing log, wrote a patch, reran the failing suite, opened `autofix/<run-id>` labelled `ai-autofix`, and it auto-merged when green.
5. Open [docs/AUTOCODING_STATS.md](AUTOCODING_STATS.md): commit counts, autofix PRs opened and merged, core coverage (about 98%), regenerated by CI on every push.

**Say**
"The owner writes prompts, not code. Tests come first, and a PR that adds a source file without a test fails the test-first check. Core packages must keep 96% coverage or the build fails. When CI breaks, an LLM sandbox reads the log and patches the code, under guardrails: it can't touch `.github/`, can't delete or skip tests, can't only edit tests, and can change at most five files. PR #1 broke the build on purpose; PR #16 is the machine fixing it. We also show the failures: thirteen `ai-autofix-failed` drafts are in the stats, because a self-healer that never fails isn't being tested."

**If it fails live:** GitHub is slow or down: show [docs/AUTOCODING.md](AUTOCODING.md) (the pipeline diagram) and the local `git log --oneline`.

## 4. Safety (2 min)

**Clicks**
1. Ops: in the scenario bar, click **Safety incident**. A snackbar reads "Safety incident injected. Agents are responding autonomously."
2. Right panel: click the **Safety** tab. Within about 15 to 20 s a new incident appears as **CHECKING IN** (route deviation of about 450 m for 10 s, plus a hard brake).
3. Click **Open rider phone**: the "HexaGo Safety · Incoming safety call" screen opens and speaks the check-in question aloud.
4. **Do not answer.** After 15 s (DEMO_MODE) the incident turns **ESCALATED**. Point at the action chips: **Driver frozen**, **Emergency notified**, **Location shared**, **Contact notified**, and the **Live location** `/track/...` link.
5. Expand the incident: **Reasoning**, **Transcript**, **Timeline**, then press play on the path replay. The vehicle leaves the planned route; the deviation band is drawn.
6. Click **Agent feed**, find the settlement event for that ride, and click **Fare breakdown**: the chips **SAFETY ESCALATION inc_…** and **PAYOUT HELD FOR REVIEW**.

**Say**
"The detector in Go watches every vehicle's telemetry. It flagged a deviation and a hard brake and sent a signal to the Safety Sentinel. The decision (monitor, check in, or escalate) is pure rules; an LLM can only upgrade it, never downgrade it, and it runs asynchronously. It called the rider. No answer in 15 seconds, so it escalated: the driver is frozen out of dispatch, emergency services and the emergency contact are notified (simulated APIs today), and a live tracking link is shared. The interrupted ride is still settled, but the driver's payout is held. Only the Ops Supervisor, after confirmation, can close the incident or release the money."

**If it fails live:** if no incident appears within 30 s, click **Safety incident** again (the scenario needs a ride on trip). If the voice doesn't play, the call screen still accepts typed replies; or show the incident from `GET https://dispatch-go.onrender.com/v1/safety/incidents?limit=5`. Optional variant: answer "I'm fine" on the call screen; the incident goes to **MONITORING**.

## 5. Rider (1.5 min)

**Clicks**
1. Back to Home, then click the **Rider** card. Under "Who is riding today?", pick **Priya Sharma**.
2. Paste the golden sentence and send. A two-leg plan appears: Anaya, school to soccer practice, Mini, 3:15 PM; Priya, office to airport, Premium, arrive by 6:00 PM with a computed pickup time. Each leg shows its fare, ETA and surge, plus the total. The footer names the model, or "rules" if the fallback answered.
3. Click **Confirm**: the legs move to **Scheduled rides**.
4. Clarifying question: type `Take my kid to soccer practice`. The agent asks "What time should we pick up Anaya?" with quick answers (Now, +30 min, +60 min). Tap one.
5. Surge: go to Ops, click **Rain storm**, and turn on the **Surge zones** legend toggle. Within about 30 s the hexes turn amber or red (up to 2.5x), and the count shows on the toggle.

**Say**
"One sentence, two passengers, two vehicle tiers, one arrive-by constraint. The Booking Intent agent resolves 'my kid', 'school' and 'office' against Priya's saved places and works backwards from 6 PM using live ETAs. If anything is missing, it asks instead of guessing. If the LLM is slow or over budget, a rule-based parser produces the same plan: turn on 'Force rule-based parser (demo)' to show it. Surge is computed per H3 cell from supply and demand; the rain storm cuts supply, and prices follow, capped."

**If it fails live:** if the LLM is slow (more than 10 s), switch on **Force rule-based parser (demo)** and resend; the same two legs come back in milliseconds. If surge zones don't light up, wait for the next 30 s recompute, or show `GET /v1/pricing/surge-map`.

## 6. Finance (1 min)

**Clicks**
1. Ops: point at the **Money** tile: **Balanced ₹0.00**, Payouts today, Held for review, Platform revenue, Rides settled today.
2. Agent feed: on any settlement (money) event, click **Fare breakdown**: Fare, Promo (funded by `promo_wallet`), Tip (100% to the driver), Rider paid, Driver 75% (or Fleet operator 80%), City levy 2% · Bengaluru, GST 5% on the platform fee, Platform fee (remainder including rounding), then **PAYOUT SENT** and the journal entries.

**Say**
"Every completed ride is one double-entry journal entry in integer paise. It sums to zero, enforced by a deferred Postgres trigger, and it's append-only, so refunds are reversal entries. The driver payout follows within 300 ms. The trial balance across every account is ₹0.00, live. The Settlement agent audits entries and payouts every minute with deterministic checks; the LLM only explains what it flags."

**If it fails live:** open `https://dispatch-go.onrender.com/v1/ledger/trial-balance` (`"total_paise":0,"balanced":true`) and `/v1/ledger/summary`.

## 7. Public API and downloads (0.5 min)

**Clicks**
1. Open https://dispatch-go.onrender.com/docs: Swagger UI, OpenAPI 3.1, about 70 operations in 10 tags.
2. Home, then **Developers**: create a key with scopes, shown once, and copy one of the curl examples.
3. Point at the SDK example in [docs/API.md](API.md) (`HexaGo.create_key` → `request_ride` → `stream`), and at **Download desktop / Android app** on Home (Windows zip and Android APK from the latest GitHub release).

**Say**
"Everything you've seen is a public API: scoped keys, per-key rate limits shared through Redis, signed webhooks, and Python and TypeScript SDKs. The same Flutter code ships to web, Windows and Android."

**If it fails live:** show [docs/openapi.json](openapi.json) and https://github.com/Sayamgada/HexaGo/releases/latest.

---

## After the demo

The suspended driver from step 4 stays frozen until it is unsuspended: `POST /v1/safety/drivers/{id}/unsuspend`, confirmed by the Ops Supervisor. That also releases the held payout.
