"""Demand Forecast agent: data source (Postgres / dispatch-go), runs, async LLM explanations, endpoint, loop."""
import asyncio
import datetime as dt

import h3
import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents.booking.gateway import GatewayError
from src.agents_py.agents.forecast import agent as fa
from src.agents_py.agents.forecast import model as fm
from src.agents_py.agents.forecast.source import ForecastSource, Loaded
from src.agents_py.config import settings
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore
from src.agents_py.main import app
from tests.helpers import FakePool, ScriptedProvider, llm_response, make_llm

T0 = dt.datetime(2026, 11, 19, 12, 30, tzinfo=dt.timezone.utc)
MG_ROAD = (12.9756, 77.6050)
OUTER = (13.0300, 77.6400)


def rows(p, minutes):
    return [{"pickup_lat": p[0], "pickup_lng": p[1], "created_at": T0 - dt.timedelta(minutes=m)} for m in minutes]


def cell(p):
    return h3.latlng_to_cell(p[0], p[1], fm.RESOLUTION)


class FakeGateway:
    def __init__(self, snapshot=None, config=None):
        self.snap = snapshot if snapshot is not None else {"drivers": [], "requests": []}
        self.config = config if config is not None else {"scenarios": [], "spikes": []}
        self.calls = 0

    async def snapshot(self, window_s=300):
        self.calls += 1
        if isinstance(self.snap, Exception):
            raise self.snap
        return self.snap

    async def sim_config(self):
        if isinstance(self.config, Exception):
            raise self.config
        return self.config


class Clock:
    def __init__(self):
        self.t = T0

    def __call__(self):
        return self.t


# ---- source ----

async def test_source_reads_recent_and_history_from_postgres():
    def handler(sql, args):
        if "generate_series" in sql:
            assert args == (T0, fm.HISTORY_DAYS)
            return [{"pickup_lat": OUTER[0], "pickup_lng": OUTER[1],
                     "created_at": T0 - dt.timedelta(days=1, minutes=5)}]
        assert args == (T0 - dt.timedelta(seconds=fm.WINDOW_S), T0)
        return rows(MG_ROAD, [1, 2, 3])

    pool = FakePool(handler)
    gw = FakeGateway(snapshot={"drivers": [{"lat": MG_ROAD[0], "lng": MG_ROAD[1], "status": "idle"}],
                               "requests": []},
                     config={"scenarios": [{"name": "rain_storm", "active_until": "2026-11-19T12:35:00.123456789Z"}]})

    async def get_pool():
        return pool

    got = await ForecastSource(gateway=gw, pool=get_pool).load(T0)
    assert got.source == "postgres" and len(got.recent) == 3 and len(got.history) == 1
    assert got.supply == {cell(MG_ROAD): 1}
    assert [s.name for s in got.scenarios] == ["rain_storm"]
    assert all("FROM rides" in s for s in pool.sql())


async def test_source_falls_back_to_the_dispatch_snapshot():
    snap = {"drivers": [], "requests": [
        {"lat": MG_ROAD[0], "lng": MG_ROAD[1], "at": "2026-11-19T12:29:00Z"},
        {"lat": MG_ROAD[0], "lng": MG_ROAD[1], "at": "garbage"},
        {"lat": "x", "lng": 1, "at": "2026-11-19T12:29:00Z"}]}

    async def down():
        return FakePool(fail=True)

    got = await ForecastSource(gateway=FakeGateway(snapshot=snap), pool=down).load(T0)
    assert got.source == "dispatch-go" and len(got.recent) == 1 and got.history == []

    async def none():
        return None

    got = await ForecastSource(gateway=FakeGateway(snapshot=GatewayError("down"), config=GatewayError("down")),
                               pool=none).load(T0)
    assert got.source == "none" and got.recent == [] and got.scenarios == [] and got.supply == {}


# ---- agent ----

class FakeSource:
    def __init__(self, loads):
        self.loads = list(loads)
        self.calls = 0

    async def load(self, now):
        self.calls += 1
        item = self.loads.pop(0) if len(self.loads) > 1 else self.loads[0]
        if isinstance(item, Exception):
            raise item
        return item


def busy(scenarios=()):
    recent = [fm.RideRequest(MG_ROAD[0], MG_ROAD[1], T0 - dt.timedelta(minutes=m)) for m in range(0, 120, 2)]
    recent += [fm.RideRequest(OUTER[0], OUTER[1], T0 - dt.timedelta(minutes=m)) for m in range(0, 120, 6)]
    return Loaded(recent=recent, history=[], scenarios=list(scenarios), supply={cell(MG_ROAD): 1}, source="postgres")


def make_agent(loads, llm=None):
    clock = Clock()
    store = EventStore(clock=clock)
    a = fa.ForecastAgent(source=FakeSource(loads), store=store, llm=llm or make_llm(enabled=False), clock=clock)
    return a, store, clock


async def test_run_logs_a_summary_event_and_serves_every_horizon():
    a, store, _ = make_agent([busy()])
    summary = await a.refresh()
    await a.idle()
    assert summary["cells"] == 2 and summary["source"] == "postgres"
    v = a.view(15)
    assert v["horizon"] == 15 and v["resolution"] == 8 and v["model"] == "holt_seasonal" and v["as_of"]
    assert v["cells"][0]["h3"] == cell(MG_ROAD) and v["hotspots"][0] in {cell(MG_ROAD), cell(OUTER)}
    assert {c["explanation_source"] for c in v["cells"]} == {"template"}
    events = [e for e in await store.recent(10) if e["metadata"].get("kind") == "forecast_run"]
    assert len(events) == 1 and events[0]["agent"] == "demand_forecast" and not events[0]["used_fallback"]
    assert "hotspot" in events[0]["decision"]
    with pytest.raises(ValueError):
        a.view(45)


async def test_empty_city_and_source_failure_keep_serving():
    empty = Loaded(recent=[], history=[], scenarios=[], supply={}, source="none")
    a, store, clock = make_agent([busy(), RuntimeError("db exploded"), empty])
    await a.refresh()
    clock.t += dt.timedelta(seconds=60)
    with pytest.raises(RuntimeError):
        await a.refresh()
    assert a.view(0)["cells"]  # the last forecast is kept
    clock.t += dt.timedelta(seconds=60)
    s = await a.refresh()
    assert s["cells"] == 0 and a.view(0)["hotspots"] == []
    assert "no ride requests" in (await store.recent(1))[0]["decision"]
    await a.idle()


async def test_llm_explains_top_hotspots_asynchronously_once():
    gate = asyncio.Event()

    async def explained(**_):
        await gate.wait()
        return llm_response({"items": [{"h3": cell(MG_ROAD), "explanation": "Office crowd heading home from MG Road."},
                                       {"h3": "8f000000000000f", "explanation": "not a hotspot, ignored"}]})

    llm = make_llm(completion=ScriptedProvider([explained]), fallback_models="", timeout_s=2)
    a, store, clock = make_agent([busy()], llm)
    await a.refresh()
    assert a.view(0)["cells"][0]["explanation_source"] == "template"  # until the LLM lands
    gate.set()
    await a.idle()
    top = next(c for c in a.view(30)["cells"] if c["h3"] == cell(MG_ROAD))
    assert top["explanation_source"] == "llm" and top["explanation"].startswith("Office crowd")
    reasoning = [e for e in await store.recent(20) if e["metadata"].get("kind") == "forecast_reasoning"]
    assert len(reasoning) == 1
    # Same hotspots a minute later: no new LLM call (the provider has nothing scripted left).
    clock.t += dt.timedelta(seconds=60)
    await a.refresh()
    await a.idle()
    assert len(llm._completion.calls) == 1
    # The explanation expires after its TTL.
    clock.t += dt.timedelta(seconds=fa.EXPLANATION_TTL_S + 1)
    assert next(c for c in a.view(0)["cells"] if c["h3"] == cell(MG_ROAD))["explanation_source"] == "template"


async def test_scenario_change_triggers_a_new_explanation_and_fallback_keeps_template():
    rain = fm.Scenario("rain_storm", T0 + dt.timedelta(minutes=5))
    a, store, clock = make_agent([busy(), busy([rain])])  # LLM disabled: fallback
    await a.refresh()
    await a.idle()
    clock.t += dt.timedelta(seconds=60)
    await a.refresh()
    await a.idle()
    v = a.view(0)
    assert v["scenarios"] == ["rain_storm"]
    assert {c["explanation_source"] for c in v["cells"]} == {"template"}
    assert "rain" in v["cells"][0]["explanation"]
    assert a._llm_calls == 2  # the scenario change re-asked despite the gap
    assert not [e for e in await store.recent(20) if e["metadata"].get("kind") == "forecast_reasoning"]


async def test_scenario_change_clears_llm_explanations():
    """No "heavy rain" line survives the end of a storm, and a late answer for the old scenarios is dropped."""
    rain = fm.Scenario("rain_storm", T0 + dt.timedelta(minutes=30))
    rainy = {"items": [{"h3": cell(MG_ROAD), "explanation": "Heavy rain near MG Road keeps riders off bikes."}]}
    gate = asyncio.Event()

    async def late(**_):
        await gate.wait()
        return llm_response(rainy)

    llm = make_llm(completion=ScriptedProvider([llm_response(rainy), late]), fallback_models="",
                   timeout_s=2, min_interval_s=0)
    a, _, clock = make_agent([busy([rain]), busy(), busy([rain]), busy()], llm)
    await a.refresh()
    await a.idle()
    top = next(c for c in a.view(15)["cells"] if c["h3"] == cell(MG_ROAD))
    assert top["explanation_source"] == "llm" and "Heavy rain" in top["explanation"]
    # The storm ends: the LLM line is cleared at once, the template no longer mentions rain.
    clock.t += dt.timedelta(seconds=60)
    await a.refresh()
    for h in fm.HORIZONS:
        for c in a.view(h)["cells"]:
            assert c["explanation_source"] == "template" and "rain" not in c["explanation"].lower()
    await a.idle()
    # Rain again, then it ends while the LLM is still answering: the stale answer is discarded.
    clock.t += dt.timedelta(seconds=60)
    await a.refresh()
    clock.t += dt.timedelta(seconds=60)
    await a.refresh()
    gate.set()
    await a.idle()
    assert len(llm._completion.calls) >= 2  # the late answer really came from the provider
    assert {c["explanation_source"] for c in a.view(15)["cells"]} == {"template"}


async def test_ended_scenario_is_not_active():
    ended = fm.Scenario("rain_storm", T0 - dt.timedelta(minutes=1))
    a, _, _ = make_agent([busy([ended])])
    await a.refresh()
    await a.idle()
    assert a.active_scenarios() == ()
    assert all("rain" not in c["explanation"] for c in a.view(0)["cells"])


async def test_explanation_errors_never_break_the_agent():
    class Exploding:
        async def complete_json(self, *a, **k):
            raise RuntimeError("boom")

    a, _, _ = make_agent([busy()], Exploding())
    await a.refresh()
    await a.idle()
    assert a.view(0)["cells"][0]["explanation_source"] == "template"


def test_stale():
    a, _, clock = make_agent([busy()])
    assert a.stale(60)
    a.as_of = clock.t
    assert not a.stale(60)
    clock.t += dt.timedelta(seconds=61)
    assert a.stale(60)


async def test_default_dependencies_are_lazy(monkeypatch):
    a = fa.ForecastAgent()
    assert isinstance(a.source, ForecastSource)
    assert await a.store() is await a.store()
    assert await a.llm() is await a.llm()
    assert fa.get_agent() is fa.get_agent()


# ---- endpoint, registry, loop ----

async def test_heatmap_endpoint_registry_and_validation(monkeypatch):
    assert registry.status_of("demand_forecast") == "active"
    a, _, _ = make_agent([busy()])
    fa.set_agent(a)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as client:
        r = await client.get("/v1/forecast/heatmap", params={"horizon": 15})
        body = r.json()
        assert r.status_code == 200 and body["horizon"] == 15 and body["interval_s"] == 60
        c = body["cells"][0]
        assert {"h3", "boundary", "demand_score", "expected_earnings_per_hr", "explanation"} <= set(c)
        calls = a.source.calls
        await client.get("/v1/forecast/heatmap")  # fresh: served from memory
        assert a.source.calls == calls
        assert (await client.get("/v1/forecast/heatmap", params={"horizon": 45})).status_code == 422
        agents = {x["id"]: x for x in (await client.get("/v1/agents")).json()["agents"]}
        assert agents["demand_forecast"]["status"] == "active"
        monkeypatch.setattr(settings, "FORECAST_ENABLED", False)
        r = await client.get("/v1/forecast/heatmap")
        assert r.status_code == 503 and "FORECAST_ENABLED" in r.json()["detail"]
    await a.idle()


async def test_loop_survives_failures_until_cancelled():
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

    with pytest.raises(asyncio.CancelledError):
        await fa.forecast_loop(60, Flaky(), sleep=sleep)
    assert len(runs) == 3


def test_apply_flag_unregisters_when_disabled(monkeypatch):
    monkeypatch.setattr(settings, "FORECAST_ENABLED", False)
    fa.apply_flag()
    assert registry.status_of("demand_forecast") == "planned"
    monkeypatch.setattr(settings, "FORECAST_ENABLED", True)
    fa.apply_flag()
    assert registry.status_of("demand_forecast") == "active"
