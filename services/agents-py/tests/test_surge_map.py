"""Surge Equilibrium surge map: pure pricing, baseline learning, alerts, async explanations, endpoint."""
import asyncio
import datetime as dt

import h3
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents.booking.gateway import GatewayError
from src.agents_py.agents.pricing import surge_map as sm
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore
from src.agents_py.main import app
from tests.helpers import ScriptedProvider, llm_response, make_llm

T0 = dt.datetime(2026, 11, 19, 6, 0, tzinfo=dt.timezone.utc)
VENUE = (12.9984, 77.5920)   # Palace Grounds
HOTSPOT = (12.9716, 77.5946)  # MG Road


def around(p, n, step=0.0004):
    """n points in a small cluster around p (well inside one res-8 disk)."""
    return [{"lat": p[0] + (i % 3 - 1) * step, "lng": p[1] + (i // 3 % 3 - 1) * step} for i in range(n)]


def snapshot(requests=(), idle=(), busy=()):
    drivers = [{**q, "status": "idle"} for q in idle] + [{**q, "status": "on_trip"} for q in busy]
    return {"drivers": drivers + [{"lat": "x", "lng": 1, "status": "idle"}],  # malformed rows are skipped
            "requests": list(requests) + [{"lat": None}]}


def steady():
    return snapshot(requests=around(HOTSPOT, 6), idle=around(HOTSPOT, 3), busy=around(HOTSPOT, 4))


def concert():
    return snapshot(requests=around(HOTSPOT, 6) + around(VENUE, 40), idle=around(HOTSPOT, 3) + around(VENUE, 1))


def top(cells, p):
    cell = h3.latlng_to_cell(p[0], p[1], sm.RESOLUTION)
    return next(c for c in cells if c["cell"] == cell)


# ---- pure pricing ----

def test_compute_cells_is_pure_and_shaped():
    a, b = sm.compute_cells(concert()), sm.compute_cells(concert())
    assert a == b
    c = top(a, VENUE)
    assert c["multiplier"] == sm.SURGE_CAP and c["label"] == "2.5x"
    assert c["demand"] == 40 and c["supply"] == 1 and c["busy"] == 0
    assert len(c["boundary"]) == 6 and all(len(pt) == 2 for pt in c["boundary"])
    assert abs(c["center"]["lat"] - VENUE[0]) < 0.01
    assert a[0]["multiplier"] >= a[-1]["multiplier"]  # highest first


def test_low_demand_never_surges():
    cells = sm.compute_cells(snapshot(requests=around(VENUE, sm.MIN_DEMAND - 1)))
    assert cells and all(c["multiplier"] == 1.0 for c in cells)
    assert sm.compute_cells({}) == []


def test_baseline_keeps_a_normally_busy_hotspot_at_one():
    stats = sm.bin_snapshot(steady())
    cell = h3.latlng_to_cell(*HOTSPOT, sm.RESOLUTION)
    plain = sm.price_cells(stats)[0]["multiplier"]
    normal = sm.price_cells(stats, {cell: sm.pressure(6, 3)})[0]["multiplier"]
    assert plain > 1.0 and normal == 1.0
    assert sm.pressure(5, 0) == 5.0


def test_template_explanation():
    c = top(sm.compute_cells(concert()), VENUE)
    text = sm.template_explanation(c)
    assert "40 ride requests" in text and "1 idle cars" in text and "2.5x" in text
    c0 = {**c, "supply": 0}
    assert "no idle car nearby" in sm.template_explanation(c0)


# ---- the agent ----

class FakeGateway:
    def __init__(self, snaps):
        self.snaps = list(snaps)
        self.calls = 0

    async def snapshot(self, window_s=300):
        self.calls += 1
        assert window_s == sm.WINDOW_S
        item = self.snaps.pop(0) if len(self.snaps) > 1 else self.snaps[0]
        if isinstance(item, Exception):
            raise item
        return item


class Clock:
    def __init__(self):
        self.t = T0

    def __call__(self):
        return self.t


def make_map(snaps, llm=None):
    clock = Clock()
    store = EventStore(clock=clock)
    m = sm.SurgeMap(gateway=FakeGateway(snaps), store=store, llm=llm or make_llm(enabled=False), clock=clock)
    return m, store, clock


async def seeded(snaps, llm=None):
    """A map whose first (baseline-seeding) look was a normal city, 30 s ago."""
    m, store, clock = make_map([steady(), *snaps], llm)
    await m.refresh()
    clock.t += dt.timedelta(seconds=30)
    return m, store, clock


async def test_first_look_seeds_the_baseline_then_concert_surges_and_alerts():
    m, store, clock = make_map([steady(), steady(), concert()])
    v = await m.refresh()  # after a restart: today's level is the norm
    assert v["surging"] == 0 and v["source"] == "dispatch-go" and v["resolution"] == 8 and v["as_of"]
    clock.t += dt.timedelta(seconds=30)
    assert (await m.refresh())["surging"] == 0
    clock.t += dt.timedelta(seconds=30)
    v = await m.refresh()
    venue = top(v["cells"], VENUE)
    assert venue["multiplier"] >= sm.ALERT_AT and v["max_multiplier"] == venue["multiplier"]
    assert venue["explanation_source"] == "template" and "40 ride requests" in venue["explanation"]
    alerts = [e for e in await store.recent(20) if e["metadata"].get("kind") == "surge_alert"]
    assert len(alerts) == 1 and alerts[0]["agent"] == "surge_equilibrium" and not alerts[0]["used_fallback"]
    assert alerts[0]["decision"].startswith(f"surge {venue['label']} in cell")
    # Still above 1.5x on the next run: no second alert.
    clock.t += dt.timedelta(seconds=30)
    await m.refresh()
    await m.idle()
    assert len([e for e in await store.recent(20) if e["metadata"].get("kind") == "surge_alert"]) == 1


async def test_llm_explanation_replaces_the_template_asynchronously():
    gate = asyncio.Event()

    async def explained(**_):
        await gate.wait()
        return llm_response({"explanation": "The concert just ended; prices ease as drivers arrive."})

    llm = make_llm(completion=ScriptedProvider([explained]), fallback_models="", timeout_s=2)
    m, store, _ = await seeded([concert()], llm)
    v = await m.refresh()
    assert top(v["cells"], VENUE)["explanation_source"] == "template"  # until the LLM lands
    gate.set()
    await m.idle()
    venue = top(m.view()["cells"], VENUE)
    assert venue["explanation_source"] == "llm" and venue["explanation"].startswith("The concert")
    reasoning = [e for e in await store.recent(20) if e["metadata"].get("kind") == "surge_reasoning"]
    assert len(reasoning) == 1 and reasoning[0]["metadata"]["model"] == "gemini/gemini-2.0-flash"


async def test_llm_fallback_keeps_template_and_logs_nothing_extra():
    m, store, _ = await seeded([concert()])  # LLM disabled
    await m.refresh()
    await m.idle()
    assert [e["metadata"]["kind"] for e in await store.recent(20)] == ["surge_alert"]


async def test_explanation_errors_never_break_the_agent():
    class Exploding:
        async def complete_json(self, *a, **k):
            raise RuntimeError("boom")

    m, store, _ = await seeded([concert()], Exploding())
    await m.refresh()
    await m.idle()
    assert top(m.view()["cells"], VENUE)["explanation_source"] == "template"


async def test_cleared_rearm_and_unavailable():
    quiet = snapshot(requests=around(VENUE, 2))
    m, store, clock = await seeded([concert(), quiet, concert(), GatewayError("down")])
    await m.refresh()
    clock.t += dt.timedelta(seconds=30)
    v = await m.refresh()
    assert v["surging"] == 0
    kinds = [e["metadata"]["kind"] for e in await store.recent(20)]
    assert kinds[0] == "surge_cleared"
    clock.t += dt.timedelta(seconds=30)
    await m.refresh()  # the venue fell below REARM_BELOW, so it alerts again
    assert [e["metadata"]["kind"] for e in await store.recent(20)].count("surge_alert") == 2
    clock.t += dt.timedelta(seconds=30)
    v = await m.refresh()
    assert v["source"] == "unavailable" and v["cells"]  # keeps the last map
    await m.idle()


async def test_baseline_forgets_cells_and_new_cells_use_the_median():
    m, _, clock = make_map([steady()])
    await m.refresh()
    assert m._default_baseline() > 1.0
    clock.t += dt.timedelta(seconds=sm.BASELINE_FORGET_S + 60)
    m._learn({}, clock.t)
    assert m.baseline == {} and m._default_baseline() == 1.0


def test_stale():
    m, _, clock = make_map([steady()])
    assert m.stale(60)
    m.as_of = clock.t
    assert not m.stale(60)
    clock.t += dt.timedelta(seconds=61)
    assert m.stale(60)


async def test_registry_endpoint_and_loop(monkeypatch):
    assert registry.status_of("surge_equilibrium") == "active"
    m, _, _ = await seeded([concert()])
    m.as_of = None  # stale: the endpoint recomputes
    sm.set_map(m)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as client:
        r = await client.get("/v1/pricing/surge-map")
        body = r.json()
        assert r.status_code == 200 and body["interval_s"] == 30 and body["surging"] >= 1
        assert top(body["cells"], VENUE)["label"] == "2.5x"
        calls = m.gateway.calls
        r = await client.get("/v1/pricing/surge-map")  # fresh: served from memory
        assert m.gateway.calls == calls
        agents = {a["id"]: a for a in (await client.get("/v1/agents")).json()["agents"]}
        assert agents["surge_equilibrium"]["status"] == "active"
    await m.idle()

    runs = []

    class Flaky:
        async def refresh(self):
            runs.append(1)
            if len(runs) == 1:
                raise RuntimeError("transient")
            return {}

    async def sleep(_):
        if len(runs) >= 3:
            raise asyncio.CancelledError

    try:
        await sm.surge_loop(30, Flaky(), sleep=sleep)
    except asyncio.CancelledError:
        pass
    assert len(runs) == 3


async def test_default_dependencies_are_lazy():
    m = sm.SurgeMap(gateway=FakeGateway([GatewayError("down")]))
    await m.refresh()
    assert (await m.store()) is not None and (await m.llm()) is not None
    assert isinstance(sm.get_map(), sm.SurgeMap)
