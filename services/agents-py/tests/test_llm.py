import asyncio
import datetime as dt

from pydantic import BaseModel

from src.agents_py.core import llm as llm_mod
from src.agents_py.core.kv import GuardStore, ist_day, next_ist_midnight
from tests.helpers import FakeClock, ScriptedProvider, embedding_response, llm_response, make_llm


class Quote(BaseModel):
    fare: float
    currency: str = "INR"


def fallback():
    return {"fare": 100.0}


async def test_success_validates_and_counts(redis, clock):
    provider = ScriptedProvider([llm_response({"fare": 245.5}, tokens=120)])
    llm = make_llm(redis, clock, completion=provider)
    res = await llm.complete_json("quote koramangala->hsr", Quote, "surge_equilibrium", fallback)
    assert res.data == {"fare": 245.5, "currency": "INR"}
    assert not res.used_fallback and res.fallback_reason is None
    assert res.tokens == 120 and not res.cached
    assert res.latency_ms >= 0
    # The prompt carries the schema and the api key is passed through.
    call = provider.calls[0]
    assert call["api_key"] == "test-key"
    assert "fare" in call["messages"][-1]["content"]
    b = await llm.budget()
    assert b["calls_today"] == 1 and b["tokens_today"] == 120


async def test_disabled_never_calls_provider(redis, clock):
    provider = ScriptedProvider()
    llm = make_llm(redis, clock, completion=provider, enabled=False)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.used_fallback and res.fallback_reason == "disabled"
    assert res.data == {"fare": 100.0, "currency": "INR"}
    assert provider.calls == []


async def test_missing_gemini_key_counts_as_disabled(redis, clock):
    provider = ScriptedProvider()
    llm = make_llm(redis, clock, completion=provider, api_key=None)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.fallback_reason == "disabled" and provider.calls == []


async def test_non_gemini_model_needs_no_gemini_key(redis, clock):
    provider = ScriptedProvider([llm_response({"fare": 1})])
    llm = make_llm(redis, clock, completion=provider, api_key=None, model="openai/gpt-4o-mini")
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert not res.used_fallback
    assert "api_key" not in provider.calls[0]


async def test_call_budget_exhaustion_falls_back(redis, clock):
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(5)])
    llm = make_llm(redis, clock, completion=provider, call_limit=2, min_interval_s=0)
    r1 = await llm.complete_json("p1", Quote, "a", fallback)
    r2 = await llm.complete_json("p2", Quote, "a", fallback)
    r3 = await llm.complete_json("p3", Quote, "a", fallback)
    assert not r1.used_fallback and not r2.used_fallback
    assert r3.used_fallback and r3.fallback_reason == "budget"
    assert len(provider.calls) == 2
    assert (await llm.budget())["calls_today"] == 2


async def test_token_budget_exhaustion_falls_back(redis, clock):
    provider = ScriptedProvider([llm_response({"fare": 1}, tokens=500), llm_response({"fare": 2})])
    llm = make_llm(redis, clock, completion=provider, token_limit=400, min_interval_s=0)
    assert not (await llm.complete_json("p1", Quote, "a", fallback)).used_fallback
    res = await llm.complete_json("p2", Quote, "a", fallback)
    assert res.fallback_reason == "budget" and len(provider.calls) == 1


async def test_budget_resets_at_ist_midnight(redis):
    # 2026-11-17 18:29:00 UTC == 23:59 IST
    clock = FakeClock(dt.datetime(2026, 11, 17, 18, 29, tzinfo=dt.timezone.utc).timestamp())
    provider = ScriptedProvider([llm_response({"fare": 1}), llm_response({"fare": 2})])
    llm = make_llm(redis, clock, completion=provider, call_limit=1, min_interval_s=0)
    assert not (await llm.complete_json("p1", Quote, "a", fallback)).used_fallback
    assert (await llm.complete_json("p2", Quote, "a", fallback)).fallback_reason == "budget"
    clock.advance(120)  # 00:01 IST next day
    assert not (await llm.complete_json("p3", Quote, "a", fallback)).used_fallback
    assert (await llm.budget())["calls_today"] == 1


def test_ist_helpers():
    t = dt.datetime(2026, 11, 17, 18, 31, tzinfo=dt.timezone.utc).timestamp()  # 00:01 IST on 18th
    assert ist_day(t) == "2026-11-18"
    midnight = dt.datetime.fromtimestamp(next_ist_midnight(t), dt.timezone.utc)
    assert midnight == dt.datetime(2026, 11, 18, 18, 30, tzinfo=dt.timezone.utc)


async def test_min_interval_per_agent(redis, clock):
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(5)])
    llm = make_llm(redis, clock, completion=provider, min_interval_s=20)
    assert not (await llm.complete_json("p1", Quote, "surge", fallback)).used_fallback
    blocked = await llm.complete_json("p2", Quote, "surge", fallback)
    assert blocked.used_fallback and blocked.fallback_reason == "rate"
    # Another agent is not throttled by surge's interval.
    assert not (await llm.complete_json("p3", Quote, "safety", fallback)).used_fallback
    await redis.delete("llm:last:surge")  # fakeredis TTLs run on wall time; emulate expiry
    assert not (await llm.complete_json("p4", Quote, "surge", fallback)).used_fallback
    assert len(provider.calls) == 3


async def test_min_interval_in_memory_uses_clock(clock):
    provider = ScriptedProvider([llm_response({"fare": i}) for i in range(3)])
    llm = make_llm(None, clock, completion=provider, min_interval_s=20)
    assert not (await llm.complete_json("p1", Quote, "surge", fallback)).used_fallback
    clock.advance(19)
    assert (await llm.complete_json("p2", Quote, "surge", fallback)).fallback_reason == "rate"
    clock.advance(2)
    assert not (await llm.complete_json("p3", Quote, "surge", fallback)).used_fallback


async def test_cache_hit_skips_provider_and_guardrails(redis, clock):
    provider = ScriptedProvider([llm_response({"fare": 321}, tokens=80)])
    llm = make_llm(redis, clock, completion=provider, min_interval_s=20)
    first = await llm.complete_json("same prompt", Quote, "surge", fallback)
    second = await llm.complete_json("same prompt", Quote, "surge", fallback)  # within min interval
    assert second.cached and not second.used_fallback
    assert second.data == first.data
    assert len(provider.calls) == 1
    b = await llm.budget()
    assert b["cache_hits"] == 1 and b["calls_today"] == 1
    # Cache expires after the TTL.
    clock.advance(601)
    await redis.flushall()  # fakeredis TTLs use wall time; emulate expiry
    provider.script.append(llm_response({"fare": 1}))
    assert not (await llm.complete_json("same prompt", Quote, "surge", fallback)).cached


async def test_cache_key_is_sha256_of_model_and_prompt(redis, clock):
    llm = make_llm(redis, clock)
    k1 = llm.cache_key("hello")
    assert len(k1.split(":")[-1]) == 64
    assert k1 != make_llm(redis, clock, model="openai/gpt-4o-mini").cache_key("hello")


async def test_timeout_retries_once_then_falls_back(redis, clock):
    async def slow(**_):
        await asyncio.sleep(1)

    provider = ScriptedProvider([slow, slow])
    llm = make_llm(redis, clock, completion=provider, timeout_s=0.05)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.used_fallback and res.fallback_reason == "timeout"
    assert len(provider.calls) == 2  # one retry
    assert provider.calls[0]["timeout"] == 0.05


async def test_retry_recovers_after_one_error(redis, clock):
    provider = ScriptedProvider([RuntimeError("429"), llm_response({"fare": 9})])
    llm = make_llm(redis, clock, completion=provider)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert not res.used_fallback and res.data["fare"] == 9
    assert (await llm.budget())["calls_today"] == 2


async def test_provider_error_falls_back(redis, clock):
    provider = ScriptedProvider([RuntimeError("boom"), RuntimeError("boom")])
    llm = make_llm(redis, clock, completion=provider)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.fallback_reason == "error"


async def test_malformed_json_is_an_error(redis, clock):
    provider = ScriptedProvider([llm_response("not json"), llm_response({"fare": "lots"})])
    llm = make_llm(redis, clock, completion=provider)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.fallback_reason == "error" and res.data["fare"] == 100.0


async def test_fenced_json_is_accepted(redis, clock):
    provider = ScriptedProvider([llm_response('```json\n{"fare": 12}\n```')])
    llm = make_llm(redis, clock, completion=provider)
    assert (await llm.complete_json("p", Quote, "a", fallback)).data["fare"] == 12


async def test_async_fallback_and_model_instance(redis, clock):
    async def afb():
        return Quote(fare=7)

    llm = make_llm(redis, clock, enabled=False)
    res = await llm.complete_json("p", Quote, "a", afb)
    assert res.data == {"fare": 7.0, "currency": "INR"}


async def test_works_without_redis(clock):
    provider = ScriptedProvider([llm_response({"fare": 1}, tokens=10), llm_response({"fare": 2})])
    llm = make_llm(None, clock, completion=provider, call_limit=1)
    assert not (await llm.complete_json("p1", Quote, "a", fallback)).used_fallback
    assert (await llm.complete_json("p1", Quote, "a", fallback)).cached
    assert (await llm.complete_json("p2", Quote, "b", fallback)).fallback_reason == "budget"
    b = await llm.budget()
    assert b["calls_today"] == 1 and b["tokens_today"] == 10 and b["cache_hits"] == 1
    assert b["store"] == "memory"


async def test_redis_failure_degrades_to_memory(clock):
    class BrokenRedis:
        def __getattr__(self, name):
            async def fail(*a, **k):
                raise ConnectionError("redis down")
            return fail

    provider = ScriptedProvider([llm_response({"fare": 1})])
    llm = make_llm(BrokenRedis(), clock, completion=provider)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert not res.used_fallback
    assert (await llm.budget())["calls_today"] == 1


async def test_budget_shape(redis, clock):
    b = await make_llm(redis, clock).budget()
    assert set(b) >= {"calls_today", "tokens_today", "limits", "enabled", "cache_hits", "model", "day"}
    assert b["limits"] == {"daily_calls": 300, "daily_tokens": 200_000, "min_interval_s": 20, "timeout_s": 0.2}
    assert b["store"] == "redis"


async def test_embed_success_and_guardrails(redis, clock):
    emb = ScriptedProvider([embedding_response([0.1, 0.2], tokens=4)])
    llm = make_llm(redis, clock, embedding=emb)
    vec, reason = await llm.embed("hello", "ops_supervisor")
    assert vec == [0.1, 0.2] and reason is None
    vec2, reason2 = await llm.embed("other", "ops_supervisor")
    assert vec2 is None and reason2 == "rate"
    vec3, _ = await llm.embed("hello", "ops_supervisor")  # cached
    assert vec3 == [0.1, 0.2]
    assert (await llm.budget())["tokens_today"] == 4


async def test_embed_disabled_and_errors(redis, clock):
    llm = make_llm(redis, clock, enabled=False)
    assert await llm.embed("x", "a") == (None, "disabled")
    emb = ScriptedProvider([RuntimeError("x"), RuntimeError("x")])
    llm = make_llm(redis, clock, embedding=emb)
    assert await llm.embed("x", "a") == (None, "error")
    llm = make_llm(redis, clock, call_limit=0)
    assert await llm.embed("x", "b") == (None, "budget")


async def test_embed_object_style_response(redis, clock):
    from types import SimpleNamespace

    resp = SimpleNamespace(data=[SimpleNamespace(embedding=[1.0])], usage=None)
    llm = make_llm(redis, clock, embedding=ScriptedProvider([resp]))
    assert (await llm.embed("x", "a"))[0] == [1.0]


async def test_module_level_client_uses_settings(monkeypatch):
    monkeypatch.setattr(llm_mod.settings, "LLM_ENABLED", False)
    llm_mod.reset_llm()
    res = await llm_mod.complete_json("p", Quote, "a", fallback)
    assert res.fallback_reason == "disabled"
    assert (await llm_mod.get_llm()).enabled is False
    llm_mod.reset_llm()


async def test_default_provider_is_litellm(monkeypatch, redis, clock):
    seen = {}

    async def fake_acompletion(**kw):
        seen.update(kw)
        return llm_response({"fare": 5})

    monkeypatch.setattr(llm_mod.litellm, "acompletion", fake_acompletion)
    llm = llm_mod.LLMClient(store=GuardStore(redis, clock=clock), clock=clock, model="gemini/x", api_key="k")
    assert (await llm.complete_json("p", Quote, "a", fallback)).data["fare"] == 5
    assert seen["model"] == "gemini/x"
