"""LLM_MODEL + LLM_FALLBACK_MODELS: 404 retires a model, 429/503 backs off and moves on."""
import asyncio

import litellm
import pytest
from pydantic import BaseModel

from src.agents_py.core import llm as llm_mod
from src.agents_py.core.llm import classify_error, parse_models
from tests.helpers import FakeSleep, ScriptedProvider, embedding_response, llm_response, make_llm

A, B, C = "gemini/a", "gemini/b", "gemini/c"


class Quote(BaseModel):
    fare: float


def fallback():
    return {"fare": 100.0}


class StatusError(Exception):
    def __init__(self, status_code, msg="provider error"):
        super().__init__(msg)
        self.status_code = status_code


def models(provider):
    return [c["model"] for c in provider.calls]


async def test_404_retires_model_and_next_one_answers(redis, clock):
    provider = ScriptedProvider([StatusError(404, "models/a is not found"), llm_response({"fare": 7}),
                                 llm_response({"fare": 8})])
    llm = make_llm(redis, clock, completion=provider, model=A, fallback_models=f"{B},{C}", min_interval_s=0)
    res = await llm.complete_json("p1", Quote, "booking", fallback)
    assert not res.used_fallback and res.model == B and res.attempts == 2
    assert llm.retired == {A}
    res2 = await llm.complete_json("p2", Quote, "booking", fallback)
    assert res2.model == B and models(provider) == [A, B, B]  # A is never called again
    b = await llm.budget()
    assert b["retired_models"] == [A] and b["fallback_models"] == [B, C]


@pytest.mark.parametrize("status", [429, 503])
async def test_transient_backs_off_then_tries_next(redis, clock, status):
    sleep = FakeSleep()
    provider = ScriptedProvider([StatusError(status), StatusError(status), llm_response({"fare": 3})])
    llm = make_llm(redis, clock, completion=provider, model=A, fallback_models=[B, C], sleep=sleep,
                   backoff_s=0.5)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.model == C and models(provider) == [A, B, C]
    assert sleep.waits == [0.5, 1.0]  # doubling backoff
    assert llm.retired == set()  # busy models are not dropped


async def test_transient_on_last_model_falls_back_without_sleeping(redis, clock):
    sleep = FakeSleep()
    provider = ScriptedProvider([StatusError(429), StatusError(429)])
    llm = make_llm(redis, clock, completion=provider, model=A, fallback_models=B, sleep=sleep)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.used_fallback and res.fallback_reason == "error"
    assert sleep.waits == [0.5]


async def test_backoff_is_capped(redis, clock):
    sleep = FakeSleep()
    chain = [f"gemini/m{i}" for i in range(6)]
    provider = ScriptedProvider([StatusError(503)] * 6)
    llm = make_llm(redis, clock, completion=provider, model=chain[0], fallback_models=chain[1:],
                   sleep=sleep, backoff_s=2)
    await llm.complete_json("p", Quote, "a", fallback)
    assert sleep.waits == [2, 4, 8, 8, 8]


async def test_timeout_moves_to_next_model(redis, clock):
    async def slow(**_):
        await asyncio.sleep(1)

    provider = ScriptedProvider([slow, llm_response({"fare": 4})])
    llm = make_llm(redis, clock, completion=provider, model=A, fallback_models=B, timeout_s=0.05)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.model == B and res.attempts == 2


async def test_single_model_chain_still_retries_once(redis, clock):
    provider = ScriptedProvider([RuntimeError("boom"), llm_response({"fare": 5})])
    llm = make_llm(redis, clock, completion=provider, model=A)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.model == A and models(provider) == [A, A]


async def test_single_model_404_is_not_retried(redis, clock):
    provider = ScriptedProvider([StatusError(404)])
    llm = make_llm(redis, clock, completion=provider, model=A, min_interval_s=0)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.fallback_reason == "error" and len(provider.calls) == 1
    # Every model retired: later calls fall back at once, with no provider call.
    res2 = await llm.complete_json("p2", Quote, "a", fallback)
    assert res2.fallback_reason == "error" and res2.attempts == 0 and len(provider.calls) == 1


async def test_malformed_output_moves_to_next_model(redis, clock):
    provider = ScriptedProvider([llm_response("not json"), llm_response({"fare": 6})])
    llm = make_llm(redis, clock, completion=provider, model=A, fallback_models=B)
    assert (await llm.complete_json("p", Quote, "a", fallback)).model == B


async def test_unusable_models_are_skipped(redis, clock):
    """No GEMINI_API_KEY: gemini models are skipped, a non-gemini fallback is used."""
    provider = ScriptedProvider([llm_response({"fare": 2})])
    llm = make_llm(redis, clock, completion=provider, api_key=None, model=A,
                   fallback_models="openai/gpt-4o-mini")
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.model == "openai/gpt-4o-mini" and models(provider) == ["openai/gpt-4o-mini"]


async def test_no_usable_model_is_disabled(redis, clock):
    llm = make_llm(redis, clock, api_key=None, model=A, fallback_models=B)
    assert (await llm.complete_json("p", Quote, "a", fallback)).fallback_reason == "disabled"


async def test_each_call_counts_against_the_budget(redis, clock):
    provider = ScriptedProvider([StatusError(404), StatusError(503), llm_response({"fare": 1})])
    llm = make_llm(redis, clock, completion=provider, model=A, fallback_models=[B, C], call_limit=2)
    res = await llm.complete_json("p", Quote, "a", fallback)
    assert res.fallback_reason == "budget" and len(provider.calls) == 2


async def test_litellm_exceptions_are_classified():
    nf = litellm.NotFoundError(message="models/gemini-2.0-flash is not found", model="x", llm_provider="gemini")
    rl = litellm.RateLimitError(message="quota", model="x", llm_provider="gemini")
    su = litellm.ServiceUnavailableError(message="overloaded", model="x", llm_provider="gemini")
    assert classify_error(nf) == "retired"
    assert classify_error(rl) == "transient"
    assert classify_error(su) == "transient"
    assert classify_error(RuntimeError("503 UNAVAILABLE")) == "transient"
    assert classify_error(RuntimeError("model is no longer available")) == "retired"
    assert classify_error(ValueError("bad json")) == "error"


def test_parse_models():
    assert parse_models(" gemini/a, ,gemini/b,gemini/a ,") == ["gemini/a", "gemini/b"]
    assert parse_models(None) == [] and parse_models("") == []
    assert parse_models(["x", "x", "y"]) == ["x", "y"]


def test_primary_is_not_repeated_in_fallbacks(redis, clock):
    llm = make_llm(redis, clock, model=A, fallback_models=f"{A},{B}")
    assert llm.fallback_models == [B] and llm.chain() == [A, B]


async def test_embedding_requests_768_dimensions(redis, clock):
    emb = ScriptedProvider([embedding_response([0.0] * 768)])
    llm = make_llm(redis, clock, embedding=emb)
    await llm.embed("hello", "a")
    assert emb.calls[0]["dimensions"] == 768
    emb2 = ScriptedProvider([embedding_response([0.0] * 768)])
    await make_llm(redis, clock, embedding=emb2, embedding_dimensions=None).embed("other text", "b")
    assert "dimensions" not in emb2.calls[0]


async def test_module_client_reads_chain_settings(monkeypatch):
    monkeypatch.setattr(llm_mod.settings, "LLM_MODEL", A)
    monkeypatch.setattr(llm_mod.settings, "LLM_FALLBACK_MODELS", f"{B}, {C}")
    monkeypatch.setattr(llm_mod.settings, "LLM_BACKOFF_S", 1.5)
    llm_mod.reset_llm()
    client = await llm_mod.get_llm()
    assert client.model == A and client.fallback_models == [B, C] and client.backoff_s == 1.5
    llm_mod.reset_llm()
