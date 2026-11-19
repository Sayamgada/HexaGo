"""End-to-end smoke test against a deployed HexaGo (default: the live Render URLs).

Prints a pass/fail table with timings and exits 1 if any check fails.

    services\\agents-py\\.venv\\Scripts\\python.exe tools\\smoke.py
    ...python.exe tools\\smoke.py --gateway http://localhost:8080 --agents http://localhost:8000 \\
        --web http://localhost:5000 --json smoke.json

Base URLs come from the flags, else HEXAGO_GATEWAY_URL / HEXAGO_AGENTS_URL /
HEXAGO_WEB_URL, else production. The WebSocket check needs `websockets`
(installed in the agents-py venv). The release check reads the GitHub API for
--repo (GITHUB_REPOSITORY, default Sayamgada/HexaGo) and uses GITHUB_TOKEN /
GH_TOKEN when set.

The checks write to the live system: they request one ride, run the
safety_incident scenario (which cancels one simulated ride and suspends its
driver until an Ops Supervisor unsuspends them) and create one API key, which
they revoke at the end.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass, field
from typing import Any, Callable

GATEWAY = "https://dispatch-go.onrender.com"
AGENTS = "https://hexago-65oz.onrender.com"
WEB = "https://hexago-web.onrender.com"
REPO = "Sayamgada/HexaGo"
USER_AGENT = "hexago-smoke/1.0"

GOLDEN_SENTENCE = ("Pick up my kid from school at 3:15 PM, drop them at soccer practice, and then book a high-tier "
                   "sedan for me from my office to the airport to arrive by 6:00 PM.")
RELEASE_ASSETS = ("HexaGo-windows-x64.zip", "HexaGo-android.apk")
PICKUP = {"lat": 12.9716, "lng": 77.5946}   # MG Road
DROPOFF = {"lat": 12.9352, "lng": 77.6245}  # Koramangala


class CheckFailed(Exception):
    """A check's expectation did not hold."""


@dataclass
class Response:
    status: int
    headers: dict[str, str]
    body: bytes

    def json(self) -> Any:
        try:
            return json.loads(self.body)
        except ValueError as err:
            raise CheckFailed(f"HTTP {self.status}: not JSON: {self.body[:120]!r}") from err


def urllib_http(method: str, url: str, body: Any = None, headers: dict[str, str] | None = None,
                timeout: float = 60) -> Response:
    data = None if body is None else json.dumps(body).encode()
    hdrs = {"User-Agent": USER_AGENT, **(headers or {})}
    if data is not None:
        hdrs["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return Response(r.status, {k.lower(): v for k, v in r.headers.items()}, r.read())
    except urllib.error.HTTPError as e:
        return Response(e.code, {k.lower(): v for k, v in e.headers.items()}, e.read())


async def _ws_first_driver_frame(url: str, topics: list[str], timeout: float) -> dict:
    import websockets  # only this check needs it

    async with websockets.connect(url, open_timeout=timeout, user_agent_header=USER_AGENT) as ws:
        await ws.send(json.dumps({"action": "subscribe", "topics": topics}))
        deadline = time.monotonic() + timeout
        while (left := deadline - time.monotonic()) > 0:
            frame = json.loads(await asyncio.wait_for(ws.recv(), timeout=left))
            if driver_in_frame(frame):
                return frame
    raise TimeoutError


def websockets_first_driver_frame(url: str, topics: list[str], timeout: float) -> dict:
    """Connects, subscribes and returns the first frame that carries a driver position."""
    try:
        return asyncio.run(_ws_first_driver_frame(url, topics, timeout))
    except (TimeoutError, asyncio.TimeoutError) as err:
        raise CheckFailed(f"no driver update within {timeout:g} s") from err


def driver_in_frame(frame: dict) -> bool:
    """True for a ride update with a driver position or a `drivers` positions frame."""
    data = frame.get("data")
    if isinstance(data, dict) and isinstance(data.get("driver"), dict):
        return "lat" in data["driver"]
    if frame.get("topic") == "drivers":
        items = data.get("drivers", data) if isinstance(data, dict) else data
        return isinstance(items, list) and len(items) > 0
    return False


@dataclass
class Ctx:
    gateway: str = GATEWAY
    agents: str = AGENTS
    web: str = WEB
    repo: str = REPO
    cold_start_s: float = 120
    safety_reply: str = "fine"  # "fine": answer the check-in; "escalate": let it time out
    http: Callable[..., Response] = urllib_http
    ws: Callable[[str, list[str], float], dict] = websockets_first_driver_frame
    sleep: Callable[[float], None] = time.sleep
    clock: Callable[[], float] = time.monotonic
    github_token: str = ""
    state: dict[str, Any] = field(default_factory=dict)

    def get(self, path: str, **kw: Any) -> Response:
        return self.http("GET", self.gateway + path, **kw)

    def post(self, path: str, body: Any = None, **kw: Any) -> Response:
        return self.http("POST", self.gateway + path, body if body is not None else {}, **kw)

    def poll(self, fn: Callable[[], Any], timeout: float, every: float = 2.0) -> Any:
        """Calls fn until it returns something truthy; None on timeout."""
        deadline = self.clock() + timeout
        while True:
            got = fn()
            if got:
                return got
            if self.clock() >= deadline:
                return None
            self.sleep(every)


def expect(cond: Any, msg: str) -> None:
    if not cond:
        raise CheckFailed(msg)


def ok_json(r: Response, *statuses: int) -> Any:
    expect(r.status in (statuses or (200,)), f"HTTP {r.status}: {r.body[:160]!r}")
    return r.json()


# ---------------------------------------------------------------- checks


def wait_healthy(ctx: Ctx, url: str) -> str:
    start = ctx.clock()
    last = ""

    def probe() -> bool:
        nonlocal last
        try:
            r = ctx.http("GET", url, timeout=30)
        except Exception as err:  # noqa: BLE001 - cold starts reset connections
            last = type(err).__name__
            return False
        last = f"HTTP {r.status}"
        return r.status == 200

    if not ctx.poll(probe, ctx.cold_start_s, every=5):
        raise CheckFailed(f"not healthy after {ctx.cold_start_s:g} s ({last})")
    return f"200 after {ctx.clock() - start:.1f} s"


def check_health_gateway(ctx: Ctx) -> str:
    return wait_healthy(ctx, ctx.gateway + "/healthz")


def check_health_agents(ctx: Ctx) -> str:
    return wait_healthy(ctx, ctx.agents + "/healthz")


def check_health_web(ctx: Ctx) -> str:
    return wait_healthy(ctx, ctx.web + "/")


def check_dispatch_p99(ctx: Ctx, limit_ms: float = 50.0) -> str:
    m = ok_json(ctx.get("/v1/metrics/dispatch"))
    p50, p99 = m.get("p50_ms"), m.get("p99_ms")
    expect(isinstance(p99, (int, float)), f"no p99_ms in {m}")
    expect(p99 <= limit_ms, f"p99 {p99} ms > {limit_ms} ms")
    return f"p50 {p50 * 1000:.0f} µs, p99 {p99 * 1000:.0f} µs, {m.get('active_drivers')} drivers"


def check_ride_matched(ctx: Ctx) -> str:
    r = ctx.post("/v1/rides", {"rider_id": "rider_smoke", "pickup": PICKUP, "dropoff": DROPOFF, "tier": "mini"})
    ride = ok_json(r, 201, 202)["ride"]
    ctx.state["ride_id"] = ride["id"]

    def matched() -> dict | None:
        cur = ok_json(ctx.get(f"/v1/rides/{ride['id']}"))
        cur = cur.get("ride", cur)
        return cur if cur.get("driver_id") else None

    got = ride if ride.get("driver_id") else ctx.poll(matched, 30)
    expect(got, f"ride {ride['id']} not matched within 30 s")
    return f"{ride['id'][:8]} -> {got['driver_id']} ({got['status']}, ETA {got.get('eta_seconds', 0):.0f} s)"


def check_stream(ctx: Ctx, timeout: float = 5.0) -> str:
    ws_url = ctx.gateway.replace("https://", "wss://").replace("http://", "ws://") + "/v1/stream"
    topics = ["drivers"] + ([f"ride:{ctx.state['ride_id']}"] if ctx.state.get("ride_id") else [])
    start = ctx.clock()
    frame = ctx.ws(ws_url, topics, timeout)
    return f"{frame.get('topic')} frame after {ctx.clock() - start:.1f} s"


def _parse_golden(ctx: Ctx, force_fallback: bool) -> str:
    q = "?force_fallback=true" if force_fallback else ""
    plan = ok_json(ctx.post(f"/v1/agents/booking/parse{q}", {"rider_id": "rider_priya", "text": GOLDEN_SENTENCE},
                            timeout=90))
    expect(plan.get("type") == "plan", f"type={plan.get('type')}: {plan.get('question') or plan}")
    legs = plan.get("legs") or []
    expect(len(legs) == 2, f"{len(legs)} legs, want 2")
    by = plan.get("answered_by") or {}
    if force_fallback:
        expect(by.get("used_fallback"), f"force_fallback but answered by {by.get('model')}")
    who = "rules" if by.get("used_fallback") else by.get("label") or by.get("model")
    why = f" ({by['fallback_reason']})" if by.get("fallback_reason") else ""
    return f"2 legs, ₹{plan.get('total_fare')}, by {who}{why}"


def check_booking_golden(ctx: Ctx) -> str:
    return _parse_golden(ctx, force_fallback=False)


def check_booking_fallback(ctx: Ctx) -> str:
    return _parse_golden(ctx, force_fallback=True)


def check_pricing_quote(ctx: Ctx) -> str:
    q = ok_json(ctx.post("/v1/pricing/quote", {"tier": "sedan", "pickup": PICKUP, "dropoff": DROPOFF}))
    expect(q.get("total", 0) > 0, f"total={q.get('total')}")
    return f"sedan ₹{q['total']} ({q.get('distance_km')} km, {q.get('surge_multiplier')}x)"


def check_surge_map(ctx: Ctx) -> str:
    m = ok_json(ctx.get("/v1/pricing/surge-map"))
    cells = m.get("cells") or []
    expect(cells, "no cells")
    return f"{len(cells)} cells, {m.get('surging')} surging, max {m.get('max_multiplier')}x"


def check_safety(ctx: Ctx) -> str:
    sc = ok_json(ctx.post("/v1/sim/scenario/safety_incident"))
    ride_id = sc.get("ride_id")
    expect(ride_id, f"scenario picked no ride: {sc.get('detail') or sc}")

    def incident() -> dict | None:
        items = ok_json(ctx.get("/v1/safety/incidents?limit=200")).get("incidents", [])
        return next((i for i in items if i.get("ride_id") == ride_id), None)

    inc = ctx.poll(incident, 90)
    expect(inc, f"no incident for ride {ride_id[:8]} within 90 s")
    if ctx.safety_reply == "fine" and inc.get("pending_checkin_id"):
        ok_json(ctx.post(f"/v1/safety/checkins/{inc['pending_checkin_id']}/reply", {"transcript": "I'm fine"}))
        inc = ctx.poll(lambda: (i := incident()) and not i.get("pending_checkin_id") and i, 30)
        expect(inc, "check-in still pending after the reply")
        expect(inc.get("decision") == "monitor", f"after 'I'm fine': {inc.get('decision')} ({inc.get('reason')})")
        return f"{inc['id']}: check-in answered 'I'm fine' -> {inc['status']}"
    if inc.get("status") != "escalated":
        inc = ctx.poll(lambda: (i := incident()) and i.get("status") == "escalated" and i, 60)
        expect(inc, "incident did not escalate within 60 s")
    flags = inc.get("flags") or {}
    expect(flags.get("driver_frozen"), f"escalated but driver not frozen: {flags}")
    return f"{inc['id']}: escalated, driver {inc.get('driver_id')} frozen"


def check_trial_balance(ctx: Ctx) -> str:
    tb = ok_json(ctx.get("/v1/ledger/trial-balance"))
    expect(tb.get("total_paise") == 0 and tb.get("balanced") is True,
           f"total_paise={tb.get('total_paise')} balanced={tb.get('balanced')}")
    return f"balanced, ₹0.00 across {sum(t.get('accounts', 0) for t in tb.get('types', []))} accounts"


def check_api_key(ctx: Ctx) -> str:
    key = ok_json(ctx.post("/v1/developer/keys", {"name": "smoke test", "scopes": ["ledger:read"]}), 200, 201)
    auth = {"Authorization": f"Bearer {key['api_key']}"}
    try:
        r = ctx.get("/v1/ledger/summary", headers=auth)
        ok_json(r)
        expect("x-ratelimit-limit" in r.headers, "no X-RateLimit-Limit header on a keyed response")
        denied = ctx.get("/v1/rides/none", headers=auth)
        expect(denied.status == 403, f"out-of-scope call answered {denied.status}, want 403")
    finally:
        ctx.http("DELETE", ctx.gateway + "/v1/developer/keys/me", headers=auth)
    return (f"{key['prefix']}: ledger summary 200 (limit {r.headers['x-ratelimit-limit']}/min), "
            "rides 403, revoked")


def check_docs(ctx: Ctx) -> str:
    d = ctx.get("/docs")
    expect(d.status == 200 and b"swagger" in d.body.lower(), f"/docs HTTP {d.status}")
    spec = ok_json(ctx.get("/openapi.json"))
    expect(str(spec.get("openapi", "")).startswith("3.1"), f"openapi={spec.get('openapi')}")
    ops = sum(1 for item in spec.get("paths", {}).values() for m in item if m in ("get", "post", "put", "delete"))
    return f"/docs 200, OpenAPI {spec['openapi']} with {ops} operations"


def check_release(ctx: Ctx) -> str:
    hdrs = {"Accept": "application/vnd.github+json"}
    if ctx.github_token:
        hdrs["Authorization"] = f"Bearer {ctx.github_token}"
    rel = ok_json(ctx.http("GET", f"https://api.github.com/repos/{ctx.repo}/releases/latest", headers=hdrs))
    assets = {a["name"]: a.get("size", 0) for a in rel.get("assets", [])}
    missing = [n for n in RELEASE_ASSETS if not assets.get(n)]
    expect(not missing, f"{rel.get('tag_name')}: missing {missing}")
    return f"{rel.get('tag_name')}: " + ", ".join(f"{n} {assets[n] / 1e6:.0f} MB" for n in RELEASE_ASSETS)


CHECKS: list[tuple[str, Callable[[Ctx], str]]] = [
    ("health: dispatch-go", check_health_gateway),
    ("health: agents-py", check_health_agents),
    ("health: web app", check_health_web),
    ("dispatch p99", check_dispatch_p99),
    ("ride created and matched", check_ride_matched),
    ("stream: driver update < 5 s", check_stream),
    ("booking: golden sentence", check_booking_golden),
    ("booking: golden, force_fallback", check_booking_fallback),
    ("pricing quote", check_pricing_quote),
    ("surge map", check_surge_map),
    ("safety incident", check_safety),
    ("ledger trial balance == 0", check_trial_balance),
    ("API key + scoped call", check_api_key),
    ("/docs and /openapi.json", check_docs),
    ("release assets", check_release),
]


# ---------------------------------------------------------------- runner


@dataclass
class Result:
    name: str
    ok: bool
    ms: float
    detail: str


def run(ctx: Ctx, checks: list[tuple[str, Callable[[Ctx], str]]] = CHECKS) -> list[Result]:
    out = []
    for name, fn in checks:
        start = time.perf_counter()
        try:
            detail, ok = fn(ctx), True
        except CheckFailed as err:
            detail, ok = str(err), False
        except Exception as err:  # noqa: BLE001 - a crash is a failed check, not a crashed run
            detail, ok = f"{type(err).__name__}: {err}", False
        out.append(Result(name, ok, (time.perf_counter() - start) * 1000, detail))
    return out


def table(results: list[Result]) -> str:
    w = max(len(r.name) for r in results)
    lines = [f"{'check':<{w}}  result  {'time':>8}  detail", f"{'-' * w}  ------  {'-' * 8}  ------"]
    for r in results:
        t = f"{r.ms / 1000:.1f} s" if r.ms >= 1000 else f"{r.ms:.0f} ms"
        lines.append(f"{r.name:<{w}}  {'PASS' if r.ok else 'FAIL':<6}  {t:>8}  {r.detail}")
    passed = sum(r.ok for r in results)
    lines.append(f"\n{passed}/{len(results)} passed in {sum(r.ms for r in results) / 1000:.1f} s")
    return "\n".join(lines)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    env = os.environ
    p = argparse.ArgumentParser(description="HexaGo end-to-end smoke test")
    p.add_argument("--gateway", default=env.get("HEXAGO_GATEWAY_URL", GATEWAY))
    p.add_argument("--agents", default=env.get("HEXAGO_AGENTS_URL", AGENTS))
    p.add_argument("--web", default=env.get("HEXAGO_WEB_URL", WEB))
    p.add_argument("--repo", default=env.get("GITHUB_REPOSITORY", REPO))
    p.add_argument("--cold-start", type=float, default=120, help="seconds to wait for each service to wake")
    p.add_argument("--safety", choices=("fine", "escalate"), default="fine",
                   help="answer the safety check-in with \"I'm fine\" or let it escalate")
    p.add_argument("--only", help="comma-separated substrings: run only matching checks")
    p.add_argument("--json", help="also write the results to this file")
    return p.parse_args(argv)


def main(argv: list[str] | None = None, **overrides: Any) -> int:
    a = parse_args(argv)
    ctx = Ctx(gateway=a.gateway.rstrip("/"), agents=a.agents.rstrip("/"), web=a.web.rstrip("/"), repo=a.repo,
              cold_start_s=a.cold_start, safety_reply=a.safety,
              github_token=os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN", ""), **overrides)
    checks = CHECKS
    if a.only:
        wanted = [s.strip().lower() for s in a.only.split(",") if s.strip()]
        checks = [c for c in CHECKS if any(s in c[0].lower() for s in wanted)]
    results = run(ctx, checks)
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")
    print(table(results))
    if a.json:
        with open(a.json, "w", encoding="utf-8") as f:
            json.dump([asdict(r) for r in results], f, indent=2, ensure_ascii=False)
    return 0 if all(r.ok for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
