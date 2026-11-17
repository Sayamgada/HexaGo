"""Real Postgres (pgvector) + Redis checks; skipped unless the URLs are set.

Point them at a pgvector Postgres (e.g. the pgvector/pgvector:pg16 image) and Redis:
  $env:HEXAGO_TEST_DATABASE_URL = "postgres://<user>:<password>@localhost:<port>/<db>?sslmode=disable"
  $env:HEXAGO_TEST_REDIS_URL = "redis://localhost:6379/15"
"""
import asyncio
import datetime as dt
import json
import os
import uuid

import pytest
from pydantic import BaseModel

from src.agents_py.core import events as ev
from src.agents_py.core import infra
from src.agents_py.core import memory as mem
from tests.helpers import ScriptedProvider, embedding_response, llm_response, make_llm

DB_URL = os.environ.get("HEXAGO_TEST_DATABASE_URL")
REDIS_URL = os.environ.get("HEXAGO_TEST_REDIS_URL")

needs_db = pytest.mark.skipif(not DB_URL, reason="HEXAGO_TEST_DATABASE_URL not set")
needs_redis = pytest.mark.skipif(not REDIS_URL, reason="HEXAGO_TEST_REDIS_URL not set")


@pytest.fixture
async def pool(monkeypatch):
    monkeypatch.setattr(infra.settings, "DATABASE_URL", DB_URL)
    p = await infra.get_pool()
    assert p is not None
    yield p
    await infra.close()


@pytest.fixture
async def real_redis(monkeypatch):
    monkeypatch.setattr(infra.settings, "REDIS_URL", REDIS_URL)
    r = await infra.get_redis()
    assert r is not None
    yield r
    await infra.close()


@needs_db
async def test_events_roundtrip_postgres(pool):
    agent = f"itest_{uuid.uuid4().hex[:8]}"
    store = ev.EventStore(pool=pool)
    await store.record_event(agent, "summary", "decided", "because", 0.75, 2.5, True, "timeout",
                             {"kind": "itest", "ride_id": "r-1", "tokens": 12})
    got = await store.recent(limit=5, agent=agent)
    assert got[0]["decision"] == "decided" and got[0]["fallback_reason"] == "timeout"
    assert got[0]["kind"] == "itest" and got[0]["ride_id"] == "r-1" and got[0]["input_summary"] == "summary"
    stats = await store.agent_stats(dt.datetime(2000, 1, 1, tzinfo=dt.timezone.utc))
    assert stats[agent]["count"] == 1 and stats[agent]["fallbacks"] == 1
    with pytest.raises(Exception, match="append-only"):
        await pool.execute("DELETE FROM agent_events WHERE agent = $1", agent)


@needs_db
async def test_memory_pgvector_and_keywords(pool, redis):
    agent = f"itest_{uuid.uuid4().hex[:8]}"

    def vec(i):
        v = [0.0] * mem.EMBEDDING_DIM
        v[i] = 1.0
        return v

    vectors = {"flood at silk board": vec(0), "hebbal hard brake": vec(1), "waterlogging hsr": vec(0)}

    async def fake_embed(**kw):
        return embedding_response(vectors[kw["input"][0]])

    llm = make_llm(redis, embedding=ScriptedProvider([fake_embed] * 3), min_interval_s=0)
    store = mem.MemoryStore(pool=pool, llm=llm)
    assert (await store.remember(agent, "flood at silk board", {"zone": "hsr"}))["embedded"]
    assert (await store.remember(agent, "hebbal hard brake"))["embedded"]
    hits = await store.recall(agent, "waterlogging hsr", k=1)
    assert hits[0]["text"] == "flood at silk board" and hits[0]["method"] == "vector"
    assert hits[0]["metadata"] == {"zone": "hsr"} and hits[0]["score"] > 0.99

    kw_store = mem.MemoryStore(pool=pool, llm=None)
    hits = await kw_store.recall(agent, "hebbal brake", k=3)
    assert hits[0]["text"] == "hebbal hard brake" and hits[0]["method"] == "keyword"
    await pool.execute("DELETE FROM agent_memory WHERE agent = $1", agent)


@needs_redis
async def test_publish_reaches_gateway_channel(real_redis):
    ps = real_redis.pubsub()
    await ps.subscribe(ev.GATEWAY_CHANNEL)
    await ps.get_message(timeout=1)
    store = ev.EventStore(redis=real_redis)
    await store.record_event("itest_agent", "", "ping", "", 1, 0, False)
    msg = None
    for _ in range(30):
        msg = await ps.get_message(ignore_subscribe_messages=True, timeout=0.1)
        if msg:
            break
        await asyncio.sleep(0)
    assert msg is not None
    topic, frame = msg["data"].split("\n", 1)
    assert topic == "ops" and json.loads(frame)["data"]["decision"] == "ping"
    await ps.aclose()


@needs_redis
async def test_llm_budget_counters_in_real_redis(real_redis):
    class Out(BaseModel):
        ok: bool

    await real_redis.flushdb()
    llm = make_llm(real_redis, completion=ScriptedProvider([llm_response({"ok": True}, tokens=33)]))
    assert not (await llm.complete_json("p", Out, "itest", lambda: {"ok": False})).used_fallback
    assert (await llm.complete_json("p2", Out, "itest", lambda: {"ok": False})).fallback_reason == "rate"
    b = await llm.budget()
    assert b["calls_today"] == 1 and b["tokens_today"] == 33 and b["store"] == "redis"
    assert await real_redis.ttl(f"llm:calls:{b['day']}") > 0
    await real_redis.flushdb()
