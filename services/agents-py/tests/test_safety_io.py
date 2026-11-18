"""Safety store (memory + Postgres write-through), dispatch-go actions client, HTTP API, supervisor review."""
import datetime as dt
import json

import httpx
import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents import ops_supervisor
from src.agents_py.agents.safety import sentinel as sentinel_mod
from src.agents_py.agents.safety.actions import ActionError, SafetyActions
from src.agents_py.agents.safety.decision import Confirmation
from src.agents_py.agents.safety.store import SafetyStore, public
from src.agents_py.core.events import EventStore
from src.agents_py.main import app
from tests.helpers import FakePool
from tests.safety_fakes import Clock, FakeActions, make_sentinel, signal

T0 = dt.datetime(2026, 11, 18, 6, 30, tzinfo=dt.timezone.utc)


# ---- store ----

def _row(inc):
    data = {k: v for k, v in inc.items() if k not in ("timeline",)}
    return {**{c: inc.get(c) for c in ("id", "ride_id", "driver_id", "rider_id", "tier", "signal_id", "severity",
                                       "status", "decision", "reason", "reasoning", "created_at", "updated_at",
                                       "resolved_at", "resolved_by", "resolution")},
            "data": json.dumps(public(data))}


async def test_store_writes_through_to_postgres_and_creates_schema():
    pool = FakePool(lambda sql, args: None)  # to_regclass -> None: tables missing
    st = SafetyStore(pool=pool, clock=Clock())
    inc = await st.create_incident(ride_id="r1", driver_id="d1", severity=0.5, status="check_in", decision="check_in")
    step = await st.add_step(inc["id"], "decision", "safety_sentinel", summary="x")
    await st.update_step(inc["id"], step["id"], reasoning="llm text")
    await st.update_step(inc["id"], "tl_missing", reasoning="ignored")
    await st.set_action(inc["id"], "driver_frozen", {"status": "ok"})
    await st.add_transcript(inc["id"], "rider", "I'm fine")
    chk = await st.create_checkin(inc["id"], "r1", "Are you okay?", 15)
    await st.update_checkin(chk["id"], status="answered")
    await st.close_incident(inc["id"], Confirmation(inc["id"], True), "all good", "ops (confirmed)")
    sql = pool.sql()
    assert any(s.startswith("CREATE TABLE IF NOT EXISTS incidents") for s in sql)
    assert sum(s.startswith("INSERT INTO incidents") for s in sql) >= 4
    assert any(s.startswith("INSERT INTO incident_timeline") for s in sql)
    assert any(s.startswith("UPDATE incident_timeline") for s in sql)
    assert sum(s.startswith("INSERT INTO safety_checkins") for s in sql) == 2
    assert st.backend == "postgres" and st.incidents[inc["id"]]["status"] == "resolved"
    # The JSON blob holds what has no column of its own.
    ins = [a for k, s, a in pool.log if s.startswith("INSERT INTO incidents")][-1]
    assert json.loads(ins[11])["actions"]["driver_frozen"] == {"status": "ok"}


async def test_store_survives_database_failures():
    st = SafetyStore(pool=FakePool(fail=True), clock=Clock())
    inc = await st.create_incident(ride_id="r1", driver_id="d1")
    assert (await st.get_incident(inc["id"]))["ride_id"] == "r1"
    assert [i["id"] for i in await st.list_incidents()] == [inc["id"]]
    assert await st.get_incident("missing") is None
    assert await st.get_checkin("missing") is None
    assert (await st.incidents_for_driver("d1"))[0]["id"] == inc["id"]


async def test_store_reads_back_from_postgres_after_restart():
    clock = Clock()
    mem = SafetyStore(clock=clock)
    inc = await mem.create_incident(ride_id="r1", driver_id="d1", severity=0.9, status="escalated",
                                    decision="escalate", actions={"driver_frozen": {"status": "ok"}})
    chk = await mem.create_checkin(inc["id"], "r1", "Are you okay?", 15)
    row = _row(inc)
    crow = {**{k: chk[k] for k in ("id", "incident_id", "ride_id", "question", "status", "timeout_s", "created_at",
                                   "deadline_at", "answered_at")}, "data": json.dumps({"transcript": None})}
    steps = [{"id": "tl_1", "step": "decision", "actor": "safety_sentinel", "details": '{"summary": "x"}', "at": T0}]

    def handler(sql, args):
        if "to_regclass" in sql:
            return "incident_timeline"
        if sql.startswith("SELECT * FROM incidents WHERE id"):
            return [row] if args[0] == inc["id"] else []
        if sql.startswith("SELECT id, step"):
            return steps
        if sql.startswith("SELECT * FROM incidents WHERE driver_id"):
            return [row]
        if sql.startswith("SELECT * FROM incidents ORDER BY"):
            return [row]
        if sql.startswith("SELECT * FROM safety_checkins"):
            return [crow] if args[0] == chk["id"] else []
        return None

    fresh = SafetyStore(pool=FakePool(handler), clock=clock)
    got = await fresh.get_incident(inc["id"])
    assert got["status"] == "escalated" and got["actions"]["driver_frozen"]["status"] == "ok"
    assert got["timeline"][0]["details"] == {"summary": "x"}
    assert await fresh.get_incident("nope") is None
    assert (await fresh.list_incidents(500))[0]["id"] == inc["id"]
    assert (await fresh.incidents_for_driver("d1"))[0]["id"] == inc["id"]
    c = await fresh.get_checkin(chk["id"])
    assert c["question"] == "Are you okay?" and c["transcript"] is None
    assert await fresh.get_checkin("nope") is None
    assert not any(s.startswith("CREATE TABLE") for s in fresh.pool.sql())  # schema already there


async def test_store_misc():
    st = SafetyStore(clock=Clock())
    assert st.backend == "memory"
    for i in range(105):
        st.record_suppressed({"i": i})
    assert len(st.suppressed) == 100 and st.suppressed[0]["i"] == 104
    st.record_escalation("d1", 0)
    st.record_escalation("d1", 100_000)  # the day-old stamp is pruned
    assert st.escalation_times("d1") == [100_000]
    assert public({"a": T0, "_hidden": 1, "b": [T0]}) == {"a": "2026-11-18T06:30:00Z", "b": ["2026-11-18T06:30:00Z"]}
    from src.agents_py.agents.safety.store import _loads
    assert _loads("not json") == {} and _loads(None) == {} and _loads(b'{"a":1}') == {"a": 1}


# ---- actions client ----

def _client(handler, redis=None):
    async def get_redis():
        return redis

    return SafetyActions("http://gw/", transport=httpx.MockTransport(handler), redis=get_redis)


async def test_actions_call_dispatch_go():
    seen = []

    def handler(req: httpx.Request):
        seen.append((req.method, req.url.path, req.url.params.get("since"), json.loads(req.content or b"null")))
        p = req.url.path
        if p == "/v1/rides/r%201" or p == "/v1/rides/r 1":
            return httpx.Response(200, json={"ride": {"id": "r 1", "status": "in_progress"}})
        if p.startswith("/v1/rides/"):
            return httpx.Response(404, json={"error": "no"})
        if p.endswith("/suspend"):
            return httpx.Response(200, json={"driver_id": "d1", "result": {"vehicle_stopped": True}})
        if p.endswith("/unsuspend"):
            return httpx.Response(200, json={"suspended": False})
        if p == "/v1/safety/track":
            return httpx.Response(201, json={"token": "t", "url": "https://gw/track/t"})
        if p.startswith("/v1/safety/emergency/"):
            return httpx.Response(200, json={"reference": "EMS-1"})
        if p.startswith("/v1/safety/vehicle-path/"):
            return httpx.Response(200, json={"points": [{"lat": 1}]})
        if p == "/v1/safety/publish":
            return httpx.Response(202, json={"published": True})
        return httpx.Response(500)

    a = _client(handler)
    assert (await a.get_ride("r 1"))["status"] == "in_progress"
    assert await a.get_ride("missing") is None
    assert (await a.suspend_driver("d1", "inc_1", "why"))["result"]["vehicle_stopped"]
    assert seen[-1][3] == {"incident_id": "inc_1", "reason": "why", "actor": "safety_sentinel"}
    conf = Confirmation("inc_1", True)
    assert (await a.unsuspend_driver("d1", "inc_1", conf))["suspended"] is False
    assert seen[-1][3]["confirmed_by"] == "ops_supervisor"
    with pytest.raises(PermissionError):
        await a.unsuspend_driver("d1", "inc_1", None)
    assert (await a.create_track_link("d1", "r1", "inc_1"))["token"] == "t"
    assert (await a.emergency_dispatch({"x": 1}))["reference"] == "EMS-1"
    assert (await a.notify_contact({"x": 1}))["reference"] == "EMS-1"
    assert await a.vehicle_path("d1", since=T0) == [{"lat": 1}]
    assert seen[-1][2] == "2026-11-18T06:30:00Z"
    assert await a.vehicle_path("d1") == [{"lat": 1}]
    assert await a.publish("ops", {"type": "safety_call"}) == "gateway"
    await a.aclose()


async def test_actions_errors_and_redis_fallback(redis):
    def fail(req):
        if req.url.path == "/v1/safety/track":
            return httpx.Response(200, content=b"not json")
        return httpx.Response(503, text="cold start")

    a = _client(fail, redis=redis)
    with pytest.raises(ActionError, match="HTTP 503"):
        await a.suspend_driver("d1", "i", "r")
    with pytest.raises(ActionError, match="invalid JSON"):
        await a.create_track_link("d1", "r", "i")
    pubsub = redis.pubsub()
    await pubsub.subscribe("hexago:stream")
    await pubsub.get_message(timeout=1)
    assert await a.publish("ride:r1", {"type": "safety_call"}) == "redis"
    msg = await pubsub.get_message(timeout=1)
    topic, frame = msg["data"].split("\n", 1)
    assert topic == "ride:r1" and json.loads(frame) == {"topic": "ride:r1", "data": {"type": "safety_call"}}

    def boom(req):
        raise httpx.ConnectError("refused")

    b = _client(boom)
    with pytest.raises(ActionError, match="ConnectError"):
        await b.get_ride("r1")
    assert await b.publish("ops", {"type": "safety_call"}) == "none"

    class BrokenRedis:
        async def publish(self, *a):
            raise ConnectionError("down")

    c = _client(boom, redis=BrokenRedis())
    assert await c.publish("ops", {}) == "none"


# ---- supervisor ----

async def test_supervisor_review_records_verdict():
    store = EventStore()
    ok = await ops_supervisor.review_resolution({"id": "inc_1", "status": "escalated"}, requested_by="ops_console",
                                                note="rider safe at home", store=store)
    assert ok.valid and ok.agent == "ops_supervisor"
    bad = await ops_supervisor.review_resolution({"id": "inc_1", "status": "escalated"},
                                                 requested_by="safety_sentinel", note="x", store=store)
    assert not bad.valid and len(bad.violations) == 2
    evs = await store.recent(agent="ops_supervisor")
    assert [e["decision"] for e in evs] == ["incident inc_1 resolution rejected", "incident inc_1 resolution confirmed"]
    assert evs[0]["metadata"]["approved"] is False


# ---- HTTP API ----

@pytest.fixture
async def api():
    actions = FakeActions()
    s, rec, clock, sleep = make_sentinel(actions)
    sentinel_mod.set_sentinel(s)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        yield client, s, actions, sleep
    sleep.fire()
    await s.idle()
    sentinel_mod.set_sentinel(None)


async def test_api_check_in_then_fine(api):
    client, s, actions, _ = api
    r = await client.post("/v1/safety/signal", json=signal(0.46))
    assert r.status_code == 202 and r.json()["action"] == "check_in"
    chk_id, inc_id = r.json()["checkin_id"], r.json()["incident_id"]
    pend = (await client.get("/v1/safety/checkins")).json()
    assert pend["count"] == 1 and pend["checkins"][0]["checkin_id"] == chk_id
    view = (await client.get(f"/v1/safety/checkins/{chk_id}")).json()
    assert view["status"] == "pending" and view["remaining_s"] == 15 and "Are you okay?" in view["question"]
    r = await client.post(f"/v1/safety/checkins/{chk_id}/reply", json={"transcript": "I'm fine"})
    assert r.status_code == 200 and r.json()["action"] == "monitor"
    r = await client.post(f"/v1/safety/checkins/{chk_id}/reply", json={"transcript": "help"})
    assert r.status_code == 409 and r.json()["status"] == "answered"
    assert (await client.get("/v1/safety/checkins?status=all")).json()["count"] == 1
    assert (await client.get("/v1/safety/checkins?status=timed_out")).json()["count"] == 0
    lst = (await client.get("/v1/safety/incidents")).json()
    assert lst["count"] == 1 and lst["incidents"][0]["status"] == "monitoring" and lst["backend"] == "memory"
    det = (await client.get(f"/v1/safety/incidents/{inc_id}")).json()
    assert [m["role"] for m in det["transcript"]] == ["agent", "rider"] and det["timeline"]


async def test_api_escalate_and_resolve(api):
    client, s, actions, _ = api
    inc_id = (await client.post("/v1/safety/signal", json=signal(0.9))).json()["incident_id"]
    await s.idle()
    det = (await client.get(f"/v1/safety/incidents/{inc_id}")).json()
    assert det["status"] == "escalated" and det["summary"]["flags"]["driver_frozen"]
    r = await client.post(f"/v1/safety/incidents/{inc_id}/resolve", json={"requested_by": "safety_sentinel",
                                                                          "note": "fine now"})
    assert r.status_code == 409 and any("Safety Sentinel" in v for v in r.json()["violations"])
    actions.fail.add("unsuspend")
    r = await client.post(f"/v1/safety/incidents/{inc_id}/resolve", json={"requested_by": "ops_console",
                                                                          "note": "rider is safe"})
    assert r.status_code == 502
    actions.fail.discard("unsuspend")
    r = await client.post(f"/v1/safety/incidents/{inc_id}/resolve", json={"requested_by": "ops_console",
                                                                          "note": "rider is safe"})
    assert r.status_code == 200 and r.json()["status"] == "resolved"


async def test_api_errors(api):
    client, *_ = api
    assert (await client.post("/v1/safety/signal", json={"ride_id": "r"})).status_code == 422
    assert (await client.post("/v1/safety/signal", json={**signal(), "severity": 2})).status_code == 422
    assert (await client.get("/v1/safety/incidents/nope")).status_code == 404
    assert (await client.post("/v1/safety/incidents/nope/resolve", json={})).status_code == 404
    assert (await client.get("/v1/safety/checkins/nope")).status_code == 404
    assert (await client.post("/v1/safety/checkins/nope/reply", json={"transcript": "hi"})).status_code == 404


async def test_safety_sentinel_registered_in_agents_py():
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        roster = {a["id"]: a for a in (await client.get("/v1/agents")).json()["agents"]}
    assert roster["safety_sentinel"]["status"] == "active" and roster["safety_sentinel"]["runs_in"] == "agents-py"


async def test_get_sentinel_singleton_picks_up_pool(monkeypatch):
    from src.agents_py.core import infra

    sentinel_mod.set_sentinel(None)
    a = await sentinel_mod.get_sentinel()
    assert a.store.pool is None
    pool = FakePool()
    monkeypatch.setattr(infra, "_pool", pool)
    b = await sentinel_mod.get_sentinel()
    assert a is b and b.store.pool is pool
    await b.actions.aclose()
    sentinel_mod.set_sentinel(None)
