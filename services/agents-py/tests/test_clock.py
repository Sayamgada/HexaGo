"""Regression: every guardrail time read goes through the injected clock.

main went red when ``test_budget_resets_at_ist_midnight`` started failing on
2026-11-18: ``GuardStore.incr`` passed an absolute ``EXPIREAT`` to Redis, which
Redis compares with its own wall clock. Once the fake clock's "today" was in the
real past, the daily counter expired the moment it was written and the budget
never tripped. These tests pin fake times far before, at and after the real
wall clock (whenever CI runs) and around IST midnight, on both backends.
"""
import datetime as dt
import time

import fakeredis
import pytest
from pydantic import BaseModel

from src.agents_py.core import kv
from src.agents_py.core import llm as llm_mod
from src.agents_py.core.kv import IST, GuardStore, ist_day, next_ist_midnight
from tests.helpers import FakeClock, ScriptedProvider, llm_response, make_llm

DAY = 86_400
# Fake "now" relative to the real wall clock: years back, now, years ahead.
WALL_OFFSETS = [-3 * 365 * DAY, -2 * DAY, 0, 2 * DAY, 3 * 365 * DAY]


class Quote(BaseModel):
    fare: float


def fallback():
    return {"fare": 100.0}


def ist(y, mo, d, h, mi, s=0) -> float:
    return dt.datetime(y, mo, d, h, mi, s, tzinfo=IST).timestamp()


def ist_midnight_near_wall(offset: float) -> float:
    """The IST midnight closest to (wall clock + offset)."""
    t = dt.datetime.fromtimestamp(time.time() + offset, IST)
    return dt.datetime(t.year, t.month, t.day, tzinfo=IST).timestamp()


@pytest.fixture(params=["redis", "memory"])
def backend(request):
    return fakeredis.FakeAsyncRedis(decode_responses=True) if request.param == "redis" else None


# ---- the bug itself ----

async def test_incr_expiry_is_relative_to_injected_clock(redis):
    """Would have caught it: a counter for a fake day in the real past must survive."""
    clock = FakeClock(ist(2020, 1, 1, 23, 0))
    store = GuardStore(redis, clock=clock)
    expire_at = next_ist_midnight(clock()) + 3600
    assert await store.incr("k", 1, expire_at) == 1
    assert await store.incr("k", 1, expire_at) == 2
    assert await store.get_int("k") == 2
    ttl = await redis.ttl("k")
    assert 0 < ttl <= 2 * 3600  # 1 h to midnight + 1 h grace, not "already expired"


async def test_incr_expiry_in_injected_past_still_sets_a_positive_ttl(redis):
    clock = FakeClock(1_800_000_000.0)
    store = GuardStore(redis, clock=clock)
    await store.incr("k", 1, int(clock()) - 10)
    assert await redis.ttl("k") >= 1  # Redis rejects 0 / negative TTLs


@pytest.mark.parametrize("offset", WALL_OFFSETS)
async def test_budget_resets_at_ist_midnight_whatever_the_wall_clock(backend, offset):
    midnight = ist_midnight_near_wall(offset)
    clock = FakeClock(midnight - 1)  # 23:59:59 IST
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(4)])
    llm = make_llm(backend, clock, completion=provider, call_limit=1, min_interval_s=0)

    assert not (await llm.complete_json("p1", Quote, "a", fallback)).used_fallback
    assert (await llm.complete_json("p2", Quote, "a", fallback)).fallback_reason == "budget"
    assert (await llm.budget())["calls_today"] == 1

    clock.t = midnight  # 00:00:00 IST exactly: a new day and a fresh budget
    assert (await llm.budget())["calls_today"] == 0
    assert not (await llm.complete_json("p3", Quote, "a", fallback)).used_fallback
    clock.advance(1)  # 00:00:01 IST
    assert (await llm.complete_json("p4", Quote, "a", fallback)).fallback_reason == "budget"
    assert (await llm.budget())["calls_today"] == 1
    assert len(provider.calls) == 2


@pytest.mark.parametrize("offset", WALL_OFFSETS)
async def test_token_budget_resets_at_ist_midnight(backend, offset):
    midnight = ist_midnight_near_wall(offset)
    clock = FakeClock(midnight - 0.5)
    provider = ScriptedProvider([llm_response({"fare": i}, tokens=500) for i in range(3)])
    llm = make_llm(backend, clock, completion=provider, token_limit=400, min_interval_s=0)
    assert not (await llm.complete_json("p1", Quote, "a", fallback)).used_fallback
    assert (await llm.complete_json("p2", Quote, "a", fallback)).fallback_reason == "budget"
    clock.t = midnight + 0.5
    assert not (await llm.complete_json("p3", Quote, "a", fallback)).used_fallback


@pytest.mark.parametrize("offset", WALL_OFFSETS)
async def test_min_interval_follows_injected_clock(backend, offset):
    clock = FakeClock(time.time() + offset)
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(3)])
    llm = make_llm(backend, clock, completion=provider, min_interval_s=20)
    assert not (await llm.complete_json("p1", Quote, "surge", fallback)).used_fallback
    clock.advance(19.9)
    assert (await llm.complete_json("p2", Quote, "surge", fallback)).fallback_reason == "rate"
    clock.advance(0.2)  # 20.1 s later: free again, though almost no real time has passed
    assert not (await llm.complete_json("p3", Quote, "surge", fallback)).used_fallback


@pytest.mark.parametrize("offset", WALL_OFFSETS)
async def test_cache_ttl_follows_injected_clock(backend, offset):
    clock = FakeClock(time.time() + offset)
    provider = ScriptedProvider([llm_response({"fare": 1}), llm_response({"fare": 2})])
    llm = make_llm(backend, clock, completion=provider, min_interval_s=0, cache_ttl_s=600)
    await llm.complete_json("same", Quote, "a", fallback)
    clock.advance(599)
    assert (await llm.complete_json("same", Quote, "a", fallback)).cached
    clock.advance(2)  # 601 s
    res = await llm.complete_json("same", Quote, "a", fallback)
    assert not res.cached and res.data["fare"] == 2


async def test_min_interval_across_ist_midnight(backend):
    """The lock is not a day key: crossing midnight does not reset it."""
    clock = FakeClock(ist(2026, 11, 18, 23, 59, 50))
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(2)])
    llm = make_llm(backend, clock, completion=provider, min_interval_s=20)
    assert not (await llm.complete_json("p1", Quote, "a", fallback)).used_fallback
    clock.advance(15)  # 00:00:05 IST next day
    assert (await llm.complete_json("p2", Quote, "a", fallback)).fallback_reason == "rate"


# ---- Redis lock details ----

async def test_lock_takeover_is_single_winner(redis):
    clock = FakeClock(1_800_000_000.0)
    a, b = GuardStore(redis, clock=clock), GuardStore(redis, clock=clock)
    assert await a.set_nx("lock", 20)
    assert not await b.set_nx("lock", 20)
    clock.advance(21)  # expired by the injected clock; Redis still holds the key
    assert await redis.exists("lock")
    assert await a.set_nx("lock", 20)
    assert not await b.set_nx("lock", 20)


async def test_legacy_lock_and_cache_values_are_treated_as_expired(redis):
    clock = FakeClock(1_800_000_000.0)
    store = GuardStore(redis, clock=clock)
    await redis.set("lock", "1", px=20_000)  # written by the previous release
    assert await store.set_nx("lock", 20)
    await redis.set("cache", '{"fare": 1}', ex=600)  # un-enveloped cache entry
    assert await store.get_json("cache") is None
    await store.set_json("cache", {"fare": 2}, 600)
    assert await store.get_json("cache") == {"fare": 2}


# ---- IST helpers at the boundary ----

def test_ist_day_and_next_midnight_at_the_boundary():
    m = ist(2026, 11, 19, 0, 0)
    assert ist_day(m - 0.001) == "2026-11-18"
    assert ist_day(m) == "2026-11-19"
    assert next_ist_midnight(m - 1) == int(m)
    assert next_ist_midnight(m) == int(m) + DAY  # at midnight, the next one is tomorrow
    assert dt.datetime.fromtimestamp(m, dt.timezone.utc) == dt.datetime(2026, 11, 18, 18, 30,
                                                                        tzinfo=dt.timezone.utc)


# ---- no stray wall-clock reads ----

class _NoWallClock:
    """Replaces the `time` module inside kv / llm: wall-clock reads fail loudly."""

    perf_counter = staticmethod(time.perf_counter)  # latency reporting only

    def __getattr__(self, name):
        raise AssertionError(f"core read time.{name}() instead of the injected clock")


async def test_core_never_reads_the_wall_clock(monkeypatch, backend):
    monkeypatch.setattr(kv, "time", _NoWallClock())
    monkeypatch.setattr(llm_mod, "time", _NoWallClock())
    clock = FakeClock(ist(2026, 11, 18, 23, 59, 59))
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(3)])
    llm = make_llm(backend, clock, completion=provider, call_limit=1, min_interval_s=5)
    await llm.complete_json("p1", Quote, "a", fallback)
    await llm.complete_json("p1", Quote, "a", fallback)  # cache
    await llm.complete_json("p2", Quote, "a", fallback)  # rate
    clock.advance(10)
    await llm.complete_json("p3", Quote, "a", fallback)  # new day, new budget
    await llm.embed("hello", "a")
    await llm.budget()
