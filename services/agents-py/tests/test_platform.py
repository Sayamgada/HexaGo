import asyncio
import datetime as dt
import time

import httpx
import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents import ops_supervisor
from src.agents_py.clients.dispatch import DRIVERS_GEO_KEY, DispatchClient, DispatchError
from src.agents_py.core import events as ev
from src.agents_py.core import graph, infra, registry
from src.agents_py.main import app
from tests.helpers import make_llm

NOW = dt.datetime(2026, 11, 17, 12, 0, tzinfo=dt.timezone.utc)


# ---- graph helpers ----

async def test_run_with_fallback_llm_path():
    node = graph.run_with_fallback(lambda s: {"decision": "llm"}, lambda s: {"decision": "fb"})
    out = await node({"agent": "x"})
    assert out["decision"] == "llm" and out["used_fallback"] is False and out["fallback_reason"] is None
    assert out["latency_ms"] >= 0


async def test_run_with_fallback_reasons():
    async def unavailable(state):
        raise graph.LLMUnavailable("budget")

    def broken(state):
        raise ValueError("bad json")

    async def fallback(state):
        return {"decision": "fb", "confidence": 0.7}

    out = await graph.run_with_fallback(unavailable, fallback)({"agent": "x"})
    assert out == {**out, "decision": "fb", "used_fallback": True, "fallback_reason": "budget"}
    out = await graph.run_with_fallback(broken, fallback)({"agent": "x"})
    assert out["fallback_reason"] == "error"


async def test_llm_node_reporting_its_own_fallback_passes_through():
    node = graph.run_with_fallback(lambda s: {"used_fallback": True, "fallback_reason": "rate"},
                                   lambda s: {"decision": "never"})
    out = await node({})
    assert out["fallback_reason"] == "rate" and "decision" not in out


async def test_build_graph_runs_end_to_end():
    node = graph.run_with_fallback(lambda s: {"decision": f"echo {s['input']['q']}"}, lambda s: {})
    g = graph.build_graph(node)
    state = await g.ainvoke({"agent": "booking_intent", "input": {"q": "hi"}})
    assert state["decision"] == "echo hi" and state["used_fallback"] is False


def test_agent_decorator_registers():
    @graph.agent("Pool Batching", "Batches pool rides")
    def pool(event):
        return event

    try:
        assert pool.agent_id == "pool_batching" and pool.agent_name == "Pool Batching"
        assert registry.registered()["pool_batching"].handler is pool
        assert registry.status_of("pool_batching") == "active"
    finally:
        registry.unregister("pool_batching")
    assert registry.status_of("pool_batching") == "planned"

    @graph.agent("custom_helper", "Not on the roster")
    def helper():
        pass

    assert registry.registered()["custom_helper"].name == "custom_helper"
    registry.unregister("custom_helper")


# ---- registry ----

async def test_list_agents_shape_and_stats():
    store = ev.EventStore(clock=lambda: NOW)
    await store.record_event("dispatch_broker", "", "matched", "near", 0.9, 1, False)
    await store.record_event("dispatch_broker", "", "no_match", "none idle", 0.0, 1, True)
    agents = await registry.list_agents(store, now=NOW)
    assert len(agents) == 11
    assert [a["id"] for a in agents] == [a.id for a in registry.ROSTER]
    by_id = {a["id"]: a for a in agents}
    db = by_id["dispatch_broker"]
    assert db["status"] == "active" and db["runs_in"] == "dispatch-go"
    assert db["decisions_last_hour"] == 2 and db["fallback_rate"] == 0.5
    assert db["last_decision"]["decision"] == "no_match"
    assert by_id["ops_supervisor"]["status"] == "active" and by_id["ops_supervisor"]["runs_in"] == "agents-py"
    assert by_id["compliance_vision"] == {**by_id["compliance_vision"], "status": "planned", "decisions_last_hour": 0,
                                   "fallback_rate": 0.0, "last_decision": None, "runs_in": None,
                                   "last_model": None}


# ---- Ops Supervisor heartbeat ----

async def test_heartbeat_is_deterministic_summary(redis):
    store = ev.EventStore(clock=lambda: NOW)
    llm = make_llm(redis)
    for i in range(12):
        await store.record_event("surge_equilibrium", "", "price", "", 0.8, 1, i < 8)
    out = await ops_supervisor.heartbeat(store=store, llm=llm, now=NOW)
    assert out["agent"] == "ops_supervisor" and out["kind"] == "heartbeat"
    assert out["used_fallback"] is False and out["confidence"] == 1.0
    assert out["decision"] == "heartbeat: attention needed"
    assert "12 decisions" in out["reasoning"] and "fallback rate 67%" in out["reasoning"]
    assert out["metadata"]["decisions_last_hour"] == 12


async def test_heartbeat_healthy_and_budget_exhausted(redis):
    store = ev.EventStore(clock=lambda: NOW)
    out = await ops_supervisor.heartbeat(store=store, llm=make_llm(redis, enabled=False), now=NOW)
    assert out["decision"] == "heartbeat: swarm healthy" and "LLM disabled" in out["reasoning"]
    out = await ops_supervisor.heartbeat(store=store, llm=make_llm(redis, call_limit=0), now=NOW)
    assert out["decision"] == "heartbeat: attention needed" and "budget exhausted" in out["reasoning"]


async def test_heartbeat_uses_shared_store_and_loop_survives_errors(monkeypatch):
    out = await ops_supervisor.heartbeat()
    assert (await (await ev.get_event_store()).recent(1))[0]["decision"] == out["decision"]

    calls = []

    async def flaky(*a, **k):
        calls.append(1)
        if len(calls) == 1:
            raise RuntimeError("db hiccup")
        if len(calls) == 3:
            raise asyncio.CancelledError

    monkeypatch.setattr(ops_supervisor, "heartbeat", flaky)
    with pytest.raises(asyncio.CancelledError):
        await ops_supervisor.heartbeat_loop(0)
    assert len(calls) == 3


# ---- HTTP endpoints ----

async def _client():
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://test")


async def test_agents_endpoint():
    await ev.record_event("ops_supervisor", "tick", "heartbeat: swarm healthy", "ok", 1.0, 0.2, False)
    async with await _client() as c:
        r = await c.get("/v1/agents")
    assert r.status_code == 200
    body = r.json()
    assert body["total"] == 11 and body["active"] >= 3 and body["events_backend"] == "memory"
    sup = next(a for a in body["agents"] if a["id"] == "ops_supervisor")
    assert sup["decisions_last_hour"] == 1 and sup["last_decision"]["decision"] == "heartbeat: swarm healthy"


async def test_events_endpoint_filters_and_limits():
    for i in range(5):
        await ev.record_event("surge_equilibrium" if i % 2 else "safety_sentinel", "", f"d{i}", "", 1, 0, False)
    async with await _client() as c:
        r = await c.get("/v1/agents/events", params={"limit": 2})
        assert [e["decision"] for e in r.json()["events"]] == ["d4", "d3"]
        r = await c.get("/v1/agents/events", params={"agent": "Surge Equilibrium"})
        assert [e["decision"] for e in r.json()["events"]] == ["d3", "d1"]
        assert (await c.get("/v1/agents/events", params={"limit": 0})).status_code == 422


async def test_budget_endpoint():
    async with await _client() as c:
        r = await c.get("/v1/agents/llm/budget")
    assert r.status_code == 200
    body = r.json()
    for key in ("calls_today", "tokens_today", "limits", "enabled", "cache_hits"):
        assert key in body
    assert body["limits"]["daily_calls"] == 300 and body["limits"]["daily_tokens"] == 200_000


async def test_healthz_reports_components():
    async with await _client() as c:
        body = (await c.get("/healthz")).json()
    assert body["components"] == {"redis": "not_configured", "postgres": "not_configured"}


def test_lifespan_starts_and_stops_heartbeat(monkeypatch):
    from fastapi.testclient import TestClient

    monkeypatch.setattr(infra.settings, "HEARTBEAT_INTERVAL_S", 3600)
    with TestClient(app) as c:
        for _ in range(50):
            events = c.get("/v1/agents/events").json()["events"]
            if events:
                break
            time.sleep(0.01)
        assert events and events[0]["agent"] == "ops_supervisor"


# ---- infra ----

async def test_infra_unreachable_services_degrade(monkeypatch):
    monkeypatch.setattr(infra.settings, "REDIS_URL", "redis://127.0.0.1:1/0")
    monkeypatch.setattr(infra.settings, "DATABASE_URL", "postgresql://u:p@127.0.0.1:1/db")
    assert await infra.get_redis() is None
    assert await infra.get_pool() is None
    assert await infra.status() == {"redis": "down", "postgres": "down"}
    await infra.close()


# ---- dispatch-go client ----

def _transport():
    def handler(request: httpx.Request):
        path = request.url.path
        if path == "/v1/rides/r1":
            return httpx.Response(200, json={"ride": {"id": "r1", "status": "matched"}})
        if path == "/v1/rides/missing":
            return httpx.Response(404, json={"error": "not found"})
        if path == "/v1/metrics/dispatch":
            return httpx.Response(200, json={"p50_ms": 0.4, "active_drivers": 300})
        return httpx.Response(500, text="boom")

    return httpx.MockTransport(handler)


async def test_dispatch_client_rest():
    async with DispatchClient("http://dispatch/", transport=_transport()) as dc:
        assert (await dc.get_ride("r1"))["ride"]["status"] == "matched"
        assert await dc.get_ride("missing") is None
        assert (await dc.get_metrics())["active_drivers"] == 300
        with pytest.raises(DispatchError, match="HTTP 500"):
            await dc.get_ride("explode")


async def test_dispatch_client_network_error():
    def handler(request):
        raise httpx.ConnectError("refused")

    async with DispatchClient("http://dispatch", transport=httpx.MockTransport(handler)) as dc:
        with pytest.raises(DispatchError, match="refused"):
            await dc.get_metrics()


async def test_drivers_near_uses_dispatch_geo_set(redis):
    await redis.geoadd(DRIVERS_GEO_KEY, [77.5946, 12.9716, "d_center", 77.6000, 12.9750, "d_close",
                                         77.7500, 13.1000, "d_far"])
    async with DispatchClient("http://dispatch", redis=redis, transport=_transport()) as dc:
        near = await dc.list_drivers_near(12.9716, 77.5946, radius_m=2000)
        assert [d["id"] for d in near] == ["d_center", "d_close"]
        assert near[0]["distance_m"] < 1 and abs(near[1]["lat"] - 12.975) < 1e-4
    async with DispatchClient("http://dispatch", transport=_transport()) as dc:
        assert await dc.list_drivers_near(12.97, 77.59) == []


async def test_drivers_near_redis_error_returns_empty():
    class Broken:
        async def geosearch(self, *a, **k):
            raise ConnectionError("down")

    async with DispatchClient("http://dispatch", redis=Broken(), transport=_transport()) as dc:
        assert await dc.list_drivers_near(12.97, 77.59) == []
