import datetime as dt
import json

from src.agents_py.core import events as ev
from src.agents_py.core import infra
from tests.helpers import FakePool

TS = dt.datetime(2026, 11, 17, 10, 0, 0, 123456, tzinfo=dt.timezone.utc)


async def _listen(redis, *channels):
    ps = redis.pubsub()
    await ps.subscribe(*channels)
    for _ in channels:  # drain subscribe confirmations
        await ps.get_message(timeout=1)
    return ps


async def _next(ps):
    for _ in range(20):
        msg = await ps.get_message(ignore_subscribe_messages=True, timeout=0.1)
        if msg:
            return msg
    raise AssertionError("no message")


def test_ops_frame_matches_gateway_contract():
    """Same shape and key order as dispatch-go stream.NewAgentEvent / Encode."""
    payload = ev.ops_payload("ops_supervisor", "heartbeat", "all quiet", 0.98765, 1.234567, False, TS)
    frame = ev.encode_frame(payload)
    assert frame == (
        '{"topic":"ops","data":{"type":"agent_event","agent":"ops_supervisor","decision":"heartbeat",'
        '"reasoning":"all quiet","confidence":0.988,"latency_ms":1.2346,"used_fallback":false,'
        '"ts":"2026-11-17T10:00:00Z"}}'
    )


def test_ops_payload_optional_fields():
    p = ev.ops_payload("safety_sentinel", "alert", "", 1, 0, True, TS, kind="incident", ride_id="r1",
                       driver_id="d1", fallback_reason="timeout")
    assert list(p)[-4:] == ["kind", "ride_id", "driver_id", "fallback_reason"]
    assert p["fallback_reason"] == "timeout"


async def test_record_event_publishes_to_ops_and_gateway_channel(redis):
    store = ev.EventStore(pool=None, redis=redis, clock=lambda: TS)
    ps = await _listen(redis, ev.OPS_CHANNEL, ev.GATEWAY_CHANNEL)
    out = await store.record_event("surge_equilibrium", "hex 89618 demand 3x", "raise 1.4x",
                                   "demand spike", 0.9, 12.5, True, "budget", {"kind": "surge", "hex": "89"})
    first, second = await _next(ps), await _next(ps)
    by_channel = {first["channel"]: first["data"], second["channel"]: second["data"]}
    frame = by_channel[ev.OPS_CHANNEL]
    data = json.loads(frame)
    assert data["topic"] == "ops"
    assert data["data"] == {
        "type": "agent_event", "agent": "surge_equilibrium", "decision": "raise 1.4x",
        "reasoning": "demand spike", "confidence": 0.9, "latency_ms": 12.5, "used_fallback": True,
        "ts": "2026-11-17T10:00:00Z", "kind": "surge", "fallback_reason": "budget",
    }
    # The gateway's Redis broker relays "<topic>\n<frame>" messages to /v1/stream subscribers.
    assert by_channel[ev.GATEWAY_CHANNEL] == "ops\n" + frame
    assert out["agent"] == "surge_equilibrium" and out["input_summary"] == "hex 89618 demand 3x"
    await ps.aclose()


async def test_memory_ring_buffer_without_database():
    store = ev.EventStore(pool=None, redis=None)
    for i in range(510):
        await store.record_event("a" if i % 2 else "b", f"in {i}", f"d{i}", "", 1.0, 0, i % 5 == 0)
    assert store.backend == "memory"
    recent = await store.recent(limit=50)
    assert len(recent) == 50 and recent[0]["decision"] == "d509"
    assert len(store.ring) == ev.RING_SIZE == 500
    only_a = await store.recent(limit=5, agent="a")
    assert [e["agent"] for e in only_a] == ["a"] * 5


async def test_record_event_survives_publish_and_db_failures():
    class BrokenRedis:
        async def publish(self, *a):
            raise ConnectionError("down")

    store = ev.EventStore(pool=FakePool(fail=True), redis=BrokenRedis())
    out = await store.record_event("x", "", "d", "", 0.5, 0, False)
    assert out["decision"] == "d"
    assert (await store.recent(10))[0]["decision"] == "d"  # read falls back to the ring


async def test_database_write_matches_dispatch_go_schema():
    pool = FakePool(lambda sql, args: 1 if "to_regclass" in sql else None)
    store = ev.EventStore(pool=pool, redis=None, clock=lambda: TS)
    await store.record_event("Booking Intent", "ride to MG Road", "parsed", "regex", 0.85, 3.0, True,
                             "disabled", {"ride_id": "r-1", "tokens": 42, "kind": "booking"})
    inserts = [(s, a) for k, s, a in pool.log if s.startswith("INSERT INTO agent_events")]
    assert len(inserts) == 1
    sql, args = inserts[0]
    assert "(agent, event_type, ride_id, input, decision, confidence, fallback_used, token_cost, latency_ms, " \
           "created_at)" in sql
    agent, event_type, ride_id, inp, decision, conf, fb, tokens, latency, created = args
    assert (agent, event_type, ride_id, conf, fb, tokens, latency, created) == (
        "booking_intent", "booking", "r-1", 0.85, True, 42, 3.0, TS)
    assert json.loads(inp)["summary"] == "ride to MG Road"
    assert json.loads(decision) == {"decision": "parsed", "reasoning": "regex", "fallback_reason": "disabled",
                                    "metadata": {"ride_id": "r-1", "tokens": 42, "kind": "booking"}}
    # Table existed, so it is not re-created.
    assert not any("CREATE TABLE" in s for s in pool.sql())


async def test_missing_table_is_created_once():
    pool = FakePool(lambda sql, args: None)  # to_regclass -> NULL
    store = ev.EventStore(pool=pool, redis=None)
    await store.record_event("a", "", "d", "", 1, 0, False)
    await store.record_event("a", "", "d", "", 1, 0, False)
    creates = [s for s in pool.sql() if "CREATE TABLE IF NOT EXISTS agent_events" in s]
    assert len(creates) == 1
    assert any("agent_events_append_only" in s for s in pool.sql())


def _db_row(i, agent="dispatch_broker", decision=None, fb=False):
    return {
        "id": i, "agent": agent, "event_type": "match", "ride_id": "r", "input": json.dumps({"summary": "s"}),
        "decision": json.dumps(decision if decision is not None else {"decision": f"d{i}", "reasoning": "near"}),
        "confidence": 0.9, "fallback_used": fb, "latency_ms": 0.5, "created_at": TS,
    }


async def test_recent_reads_database_rows():
    rows = [_db_row(2), _db_row(1, decision="bare string"), _db_row(0, decision={"x": 1})]
    pool = FakePool(lambda sql, args: rows if "FROM agent_events" in sql else 1)
    store = ev.EventStore(pool=pool, redis=None)
    out = await store.recent(limit=3, agent="dispatch_broker")
    assert out[0]["decision"] == "d2" and out[0]["reasoning"] == "near"
    assert out[0]["input_summary"] == "s" and out[0]["kind"] == "match"
    assert out[0]["ts"] == "2026-11-17T10:00:00Z" and out[0]["id"] == 2
    assert out[1]["decision"] == "bare string"
    assert out[2]["decision"] == "match" and out[2]["reasoning"] == '{"x": 1}'
    fetch = [a for k, s, a in pool.log if "FROM agent_events" in s][0]
    assert fetch == ("dispatch_broker", 3)


async def test_agent_stats_from_memory_and_db():
    now = TS
    store = ev.EventStore(pool=None, redis=None, clock=lambda: now)
    await store.record_event("Dispatch Broker", "", "m1", "", 1, 0, False)
    await store.record_event("dispatch_broker", "", "m2", "", 1, 0, True)
    await store.record_event("booking_intent", "", "b1", "", 1, 0, False, metadata={"model": "gemini/x"})
    await store.record_event("booking_intent", "", "b2", "", 1, 0, False)
    stats = await store.agent_stats(now - dt.timedelta(hours=1))
    assert stats["dispatch_broker"]["count"] == 2 and stats["dispatch_broker"]["fallbacks"] == 1
    assert stats["dispatch_broker"]["last"]["decision"] == "m2" and stats["dispatch_broker"]["model"] is None
    assert stats["booking_intent"]["model"] == "gemini/x" and stats["booking_intent"]["last"]["decision"] == "b2"

    def handler(sql, args):
        if "GROUP BY agent" in sql:
            return [{"agent": "safety_sentinel", "n": 4, "fallbacks": 4}]
        if "AS model" in sql:
            assert args[0] == now - dt.timedelta(days=1)
            return [{"agent": "safety_sentinel", "model": "gemini/gemini-3.5-flash-lite"}, {"agent": "x", "model": None}]
        if "DISTINCT ON" in sql:
            return [_db_row(9, agent="safety_sentinel"), _db_row(8, agent="fleet_simulator")]
        return 1

    store = ev.EventStore(pool=FakePool(handler), redis=None)
    stats = await store.agent_stats(now)
    assert stats["safety_sentinel"]["count"] == 4 and stats["safety_sentinel"]["last"]["decision"] == "d9"
    assert stats["fleet_simulator"]["count"] == 0
    assert stats["safety_sentinel"]["model"] == "gemini/gemini-3.5-flash-lite" and "x" not in stats


async def test_agent_stats_db_failure_uses_ring():
    store = ev.EventStore(pool=FakePool(fail=True), redis=None)
    await store.record_event("a", "", "d", "", 1, 0, False)
    assert (await store.agent_stats(TS - dt.timedelta(days=1)))["a"]["count"] == 1


async def test_module_level_record_event_uses_infra(redis):
    infra.override(redis=redis, pool=None)
    ev.reset_event_store()
    await ev.record_event("ops_supervisor", "tick", "heartbeat", "ok", 1.0, 0.1, False)
    events = await (await ev.get_event_store()).recent(5)
    assert events[0]["decision"] == "heartbeat"
    ev.reset_event_store()


def test_agent_id_normalisation():
    assert ev.agent_id("Legal & Jurisdiction") == "legal_jurisdiction"
    assert ev.agent_id("dispatch_broker") == "dispatch_broker"
    assert ev.agent_id("Ops Supervisor") == "ops_supervisor"


def _struct_row(event_type, decision, ride_id="r-9"):
    return {"id": 1, "agent": "dispatch_broker", "event_type": event_type, "ride_id": ride_id,
            "input": "{}", "decision": json.dumps(decision), "confidence": 0.55, "fallback_used": False,
            "latency_ms": 0.1, "created_at": TS}


def test_dispatch_go_structured_rows_are_summarised():
    match = ev._from_row(_struct_row("match", {
        "driver_id": "d_196", "matched_tier": "sedan", "requested_tier": "mini", "upgraded": True,
        "rings_searched": 14, "search_radius_km": 4.87, "candidates_considered": 5,
        "score": {"eta_seconds": 417.2, "distance_km": 2.086, "total": 0.554}, "alternatives": []}))
    assert match["decision"] == "matched ride r-9 to d_196 (sedan), pickup ETA 7.0 min, upgraded from mini"
    assert match["reasoning"] == "Best of 5 candidates within 14 H3 rings (~4.9 km): 2.09 km away, score 0.55."
    assert match["metadata"]["driver_id"] == "d_196"

    no_match = ev._from_row(_struct_row("no_match", {
        "rings_searched": 1, "search_radius_km": 5.0, "candidates_found": 0, "requested_tier": "suv"}))
    assert no_match["decision"] == "no driver available for ride r-9 (suv); request left open"
    assert no_match["reasoning"] == "Searched 1 H3 ring (~5.0 km); 0 candidates found."

    other = ev._from_row(_struct_row("scenario", {"name": "rush_hour"}, ride_id=None))
    assert other["decision"] == "scenario" and other["reasoning"] == '{"name": "rush_hour"}'
