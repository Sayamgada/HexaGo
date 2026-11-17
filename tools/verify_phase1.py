"""Phase 1 live verification for the HexaGo Ops Command Center.

Checks the deployed gateway, agents service and web app, then measures the
live demo for a window (default 2 minutes) over REST and the /v1/stream
WebSocket, and finally triggers the rush_hour scenario and watches it for 20 s.

Reports:
  * steady-state busy share  = (active - idle) / active drivers
  * match success over the window: first try, and eventual (incl. re-dispatch)
  * match latency p50 / p99 (gateway metrics, sub-millisecond floats)
  * what rush_hour did within 20 s

Run with the agents-py venv (it has `websockets`):

    services\\agents-py\\.venv\\Scripts\\python.exe tools\\verify_phase1.py
    ...python.exe tools\\verify_phase1.py --window 60 --no-scenario --json out.json
"""

from __future__ import annotations

import argparse
import asyncio
import json
import statistics
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass, field

import websockets

GATEWAY = "https://dispatch-go.onrender.com"
AGENTS = "https://hexago-65oz.onrender.com"
WEB = "https://hexago-web.onrender.com"
USER_AGENT = "hexago-verify/1.0"


# ---------------------------------------------------------------- HTTP helpers


def http(method: str, url: str, timeout: float = 30) -> tuple[int, bytes]:
    req = urllib.request.Request(url, method=method, headers={"User-Agent": USER_AGENT})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def get_json(url: str) -> dict:
    status, body = http("GET", url)
    if status != 200:
        raise RuntimeError(f"GET {url} -> {status}: {body[:200]!r}")
    return json.loads(body)


def wait_healthy(url: str, deadline_s: float = 240) -> tuple[bool, float, str]:
    """Polls url until HTTP 200 (Render free instances cold-start slowly)."""
    start = time.monotonic()
    last = ""
    while time.monotonic() - start < deadline_s:
        try:
            status, body = http("GET", url, timeout=60)
            if status == 200:
                return True, time.monotonic() - start, body[:300].decode(errors="replace")
            last = f"HTTP {status}"
        except Exception as e:  # noqa: BLE001 - report any network failure
            last = str(e)
        time.sleep(5)
    return False, time.monotonic() - start, last


# ---------------------------------------------------------------- measurement


@dataclass
class Window:
    seconds: float = 0
    busy_samples: list[float] = field(default_factory=list)
    new_requests: int = 0
    first_try: int = 0
    redispatched: int = 0
    unmatched: int = 0
    upgraded: int = 0
    match_latencies_ms: list[float] = field(default_factory=list)
    events_by_agent: dict[str, int] = field(default_factory=dict)
    incidents: int = 0
    with_kind: int = 0  # dispatch events carrying the structured `kind` field

    @property
    def busy(self) -> float:
        return statistics.mean(self.busy_samples) if self.busy_samples else float("nan")

    @property
    def first_try_rate(self) -> float:
        return self.first_try / self.new_requests if self.new_requests else float("nan")

    @property
    def eventual_rate(self) -> float:
        if not self.new_requests:
            return float("nan")
        return min(1.0, (self.first_try + self.redispatched) / self.new_requests)


def busy_share(m: dict) -> float:
    active = m.get("active_drivers") or 0
    return (active - (m.get("idle_drivers") or 0)) / active if active else float("nan")


def classify(ev: dict, w: Window) -> None:
    agent = ev.get("agent", "?")
    w.events_by_agent[agent] = w.events_by_agent.get(agent, 0) + 1
    kind = ev.get("kind", "")
    decision = ev.get("decision", "")
    if agent == "safety_sentinel" or kind == "incident":
        w.incidents += 1
    if agent != "dispatch_broker":
        return
    if kind:
        w.with_kind += 1
    if kind == "no_match" or decision.startswith("no driver"):
        w.new_requests += 1
        w.unmatched += 1
    elif kind == "match" or decision.startswith("matched"):
        if "re-dispatched" in decision:
            w.redispatched += 1
        else:
            w.new_requests += 1
            w.first_try += 1
        if isinstance(ev.get("latency_ms"), (int, float)):
            w.match_latencies_ms.append(float(ev["latency_ms"]))
    if ev.get("upgraded"):
        w.upgraded += 1


async def observe(gateway: str, seconds: float) -> tuple[Window, dict]:
    """Streams ops for `seconds`; busy share sampled from ops metrics frames."""
    ws_url = gateway.replace("https://", "wss://").replace("http://", "ws://") + "/v1/stream"
    w = Window()
    last_metrics: dict = {}
    last_sample = 0.0
    async with websockets.connect(ws_url, user_agent_header=USER_AGENT, open_timeout=60, max_size=8 << 20) as ws:
        await ws.send(json.dumps({"action": "subscribe", "topics": ["ops"]}))
        start = time.monotonic()
        while (elapsed := time.monotonic() - start) < seconds:
            try:
                raw = await asyncio.wait_for(ws.recv(), timeout=max(0.1, seconds - elapsed))
            except TimeoutError:
                break
            msg = json.loads(raw)
            if msg.get("topic") != "ops":
                continue
            data = msg.get("data") or {}
            if data.get("type") == "agent_event":
                classify(data, w)
            elif data.get("type") == "metrics":
                last_metrics = data
                if time.monotonic() - last_sample >= 5:
                    w.busy_samples.append(busy_share(data))
                    last_sample = time.monotonic()
        w.seconds = time.monotonic() - start
    return w, last_metrics


def pct(v: float) -> str:
    return "n/a" if v != v else f"{v * 100:.1f}%"


def fmt_ms(ms: float) -> str:
    return f"{ms * 1000:.0f} µs" if ms < 1 else f"{ms:.2f} ms"


# ---------------------------------------------------------------- main


@dataclass
class Check:
    name: str
    ok: bool
    detail: str


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--gateway", default=GATEWAY)
    ap.add_argument("--agents", default=AGENTS)
    ap.add_argument("--web", default=WEB)
    ap.add_argument("--window", type=float, default=120, help="steady-state window in seconds")
    ap.add_argument("--warmup", type=float, default=0, help="extra seconds to wait before measuring")
    ap.add_argument("--no-scenario", action="store_true", help="skip triggering rush_hour")
    ap.add_argument("--json", help="also write the report to this file")
    args = ap.parse_args()
    gw = args.gateway.rstrip("/")
    checks: list[Check] = []
    report: dict = {"gateway": gw, "agents": args.agents, "web": args.web}

    # 1. Liveness (cold starts allowed).
    for name, url in [("gateway /healthz", gw + "/healthz"), ("agents /healthz", args.agents.rstrip("/") + "/healthz"),
                      ("web app", args.web.rstrip("/") + "/")]:
        ok, took, detail = wait_healthy(url)
        checks.append(Check(name, ok, f"{'up' if ok else 'DOWN'} after {took:.0f}s" + ("" if ok else f": {detail}")))
        print(f"[{'ok' if ok else 'FAIL'}] {name}: {checks[-1].detail}", flush=True)
    if not checks[0].ok:
        print("gateway is down; stopping")
        return 1

    sim = get_json(gw + "/v1/sim/config")
    report["sim"] = {k: sim.get(k) for k in ("drivers", "live_drivers", "demand_per_min", "current_demand_per_min",
                                             "time_of_day_factor", "speed_factor", "router", "scenarios")}
    print(f"sim: {sim.get('live_drivers')} drivers, base {sim.get('demand_per_min')}/min, "
          f"now {sim.get('current_demand_per_min')}/min (tod {sim.get('time_of_day_factor')}), router {sim.get('router')}")
    if args.warmup:
        print(f"warming up {args.warmup:.0f}s ...", flush=True)
        time.sleep(args.warmup)

    # 2. Steady state.
    print(f"observing /v1/stream for {args.window:.0f}s ...", flush=True)
    w, _ = asyncio.run(observe(gw, args.window))
    m = get_json(gw + "/v1/metrics/dispatch")
    report["steady_state"] = {
        **{k: v for k, v in asdict(w).items() if k != "match_latencies_ms"},
        "busy_share": w.busy, "first_try_rate": w.first_try_rate, "eventual_rate": w.eventual_rate,
        "event_latency_ms_p50": statistics.median(w.match_latencies_ms) if w.match_latencies_ms else None,
        "metrics": m,
    }
    p50, p99 = float(m["p50_ms"]), float(m["p99_ms"])
    print(f"busy share {pct(w.busy)} over {len(w.busy_samples)} samples; "
          f"{w.new_requests} new requests: first try {pct(w.first_try_rate)}, eventually {pct(w.eventual_rate)} "
          f"({w.redispatched} re-dispatched, {w.unmatched} left open, {w.upgraded} upgrades)")
    print(f"match latency p50 {fmt_ms(p50)} / p95 {fmt_ms(float(m['p95_ms']))} / p99 {fmt_ms(p99)}; "
          f"raw JSON p50_ms={m['p50_ms']!r}")
    checks.append(Check("busy share 40-60%", 0.40 <= w.busy <= 0.60, pct(w.busy)))
    checks.append(Check("match p50 < 5 ms", 0 < p50 < 5, fmt_ms(p50)))
    checks.append(Check("latency is a sub-ms float (not 0.0)", isinstance(m["p50_ms"], float) and p50 > 0, repr(m["p50_ms"])))
    checks.append(Check("requests flowing", w.new_requests > 0, f"{w.new_requests} in {w.seconds:.0f}s"))
    checks.append(Check("agent_events carry kind/ride_id", w.with_kind > 0, f"{w.with_kind} dispatch events with kind"))

    # 3. rush_hour.
    if not args.no_scenario:
        before = get_json(gw + "/v1/metrics/dispatch")
        rate_before = get_json(gw + "/v1/sim/config").get("current_demand_per_min")
        status, body = http("POST", gw + "/v1/sim/scenario/rush_hour")
        view = json.loads(body) if body else {}
        print(f"rush_hour -> HTTP {status}: {view.get('detail')}", flush=True)
        rw, _ = asyncio.run(observe(gw, 20))
        after = get_json(gw + "/v1/metrics/dispatch")
        rate_after = get_json(gw + "/v1/sim/config").get("current_demand_per_min")
        report["rush_hour"] = {
            "http_status": status, "detail": view.get("detail"),
            "demand_per_min_before": rate_before, "demand_per_min_after": rate_after,
            "busy_before": busy_share(before), "busy_after_20s": busy_share(after),
            "idle_before": before["idle_drivers"], "idle_after_20s": after["idle_drivers"],
            "open_before": before["open_requests"], "open_after_20s": after["open_requests"],
            "dispatch_events_20s": rw.events_by_agent.get("dispatch_broker", 0),
            "new_requests_20s": rw.new_requests, "first_try_rate_20s": rw.first_try_rate,
        }
        r = report["rush_hour"]
        print(f"rush_hour after 20 s: demand {rate_before} -> {rate_after}/min; busy {pct(r['busy_before'])} -> "
              f"{pct(r['busy_after_20s'])}; idle {r['idle_before']} -> {r['idle_after_20s']}; open "
              f"{r['open_before']} -> {r['open_after_20s']}; {r['new_requests_20s']} requests in 20 s")
        checks.append(Check("rush_hour accepted", status == 200, f"HTTP {status}"))
        checks.append(Check("rush_hour visible within 20 s",
                            rw.new_requests >= 20 and busy_share(after) > busy_share(before),
                            f"{rw.new_requests} requests, busy {pct(busy_share(before))} -> {pct(busy_share(after))}"))

    report["checks"] = [asdict(c) for c in checks]
    print("\nchecks:")
    for c in checks:
        print(f"  [{'PASS' if c.ok else 'FAIL'}] {c.name}: {c.detail}")
    if args.json:
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump(report, f, indent=2, default=str)
    return 0 if all(c.ok for c in checks) else 1


if __name__ == "__main__":
    sys.exit(main())
