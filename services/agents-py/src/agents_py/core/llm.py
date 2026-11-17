"""LiteLLM wrapper with mandatory cost guardrails.

Every agent reaches the LLM through `complete_json` (or `embed`). Before a
provider call it checks, in order:

1. ``LLM_ENABLED`` (and that a Gemini key exists for ``gemini/*`` models) -> "disabled"
2. the response cache (sha256 of model + prompt, 10 min TTL) -> served without a call
3. the per-agent minimum interval ``LLM_MIN_INTERVAL_S``      -> "rate"
4. the daily call and token budgets (reset at midnight IST)   -> "budget"

A provider timeout (``LLM_TIMEOUT_S``) or error is retried once, then becomes
"timeout" / "error". Whenever anything blocks or fails, the caller's
deterministic fallback runs and the result is marked ``used_fallback=True``.

Agents call this only on meaningful events (a booking request, an incident, an
unusual surge), never on every tick; the min interval enforces that.
"""
from __future__ import annotations

import asyncio
import hashlib
import inspect
import json
import logging
import re
import time
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable, Dict, List, Optional, Tuple, Type, Union

import litellm
from prometheus_client import Counter
from pydantic import BaseModel

from src.agents_py.config import settings
from src.agents_py.core import infra
from src.agents_py.core.kv import GuardStore, ist_day, next_ist_midnight

log = logging.getLogger("hexago.llm")

FallbackFn = Callable[[], Union[Dict[str, Any], BaseModel, Awaitable[Union[Dict[str, Any], BaseModel]]]]

LLM_CALLS = Counter("hexago_llm_calls_total", "LLM requests by outcome", ["agent", "outcome"])
LLM_TOKENS = Counter("hexago_llm_tokens_total", "LLM tokens consumed", ["agent"])

FALLBACK_REASONS = ("disabled", "budget", "rate", "timeout", "error")


@dataclass
class LLMResult:
    data: Dict[str, Any]
    used_fallback: bool
    fallback_reason: Optional[str] = None
    cached: bool = False
    latency_ms: float = 0.0
    tokens: int = 0
    model: str = ""
    attempts: int = 0
    extra: Dict[str, Any] = field(default_factory=dict)


class _Blocked(Exception):
    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


def _parse_json(text: str) -> Any:
    text = (text or "").strip()
    fenced = re.match(r"^```(?:json)?\s*(.*?)\s*```$", text, re.S)
    if fenced:
        text = fenced.group(1)
    return json.loads(text)


def _tokens(resp: Any) -> int:
    usage = getattr(resp, "usage", None)
    if usage is None and isinstance(resp, dict):
        usage = resp.get("usage")
    if usage is None:
        return 0
    total = usage.get("total_tokens") if isinstance(usage, dict) else getattr(usage, "total_tokens", 0)
    return int(total or 0)


class LLMClient:
    def __init__(
        self,
        *,
        store: GuardStore,
        model: str = "gemini/gemini-2.0-flash",
        api_key: Optional[str] = None,
        enabled: bool = True,
        timeout_s: float = 3.0,
        call_limit: int = 300,
        token_limit: int = 200_000,
        min_interval_s: float = 20.0,
        cache_ttl_s: int = 600,
        embedding_model: str = "gemini/text-embedding-004",
        completion: Optional[Callable[..., Awaitable[Any]]] = None,
        embedding: Optional[Callable[..., Awaitable[Any]]] = None,
        clock: Callable[[], float] = time.time,
    ):
        self.store = store
        self.model = model
        self.api_key = api_key
        self.enabled = enabled
        self.timeout_s = timeout_s
        self.call_limit = call_limit
        self.token_limit = token_limit
        self.min_interval_s = min_interval_s
        self.cache_ttl_s = cache_ttl_s
        self.embedding_model = embedding_model
        self._completion = completion
        self._embedding = embedding
        self.clock = clock

    # ---- keys ----
    def _day_key(self, name: str) -> str:
        return f"llm:{name}:{ist_day(self.clock())}"

    def cache_key(self, prompt: str, model: Optional[str] = None) -> str:
        digest = hashlib.sha256(f"{model or self.model}{prompt}".encode()).hexdigest()
        return f"llm:cache:{digest}"

    # ---- guardrails ----
    def _usable(self, model: str) -> bool:
        if not self.enabled:
            return False
        return not (model.startswith("gemini/") and not self.api_key)

    async def _check_rate(self, agent: str) -> None:
        if not await self.store.set_nx(f"llm:last:{agent}", self.min_interval_s):
            raise _Blocked("rate")

    async def _reserve_call(self) -> None:
        """Count one provider call against today's budget, or raise budget."""
        if await self.store.get_int(self._day_key("tokens")) >= self.token_limit:
            raise _Blocked("budget")
        expire = next_ist_midnight(self.clock()) + 3600
        if await self.store.incr(self._day_key("calls"), 1, expire) > self.call_limit:
            await self.store.incr(self._day_key("calls"), -1, expire)
            raise _Blocked("budget")

    async def _add_tokens(self, agent: str, tokens: int) -> None:
        if tokens:
            await self.store.incr(self._day_key("tokens"), tokens, next_ist_midnight(self.clock()) + 3600)
            LLM_TOKENS.labels(agent).inc(tokens)

    async def _cache_hit(self) -> None:
        await self.store.incr(self._day_key("cache_hits"), 1, next_ist_midnight(self.clock()) + 3600)

    # ---- public API ----
    async def complete_json(
        self, prompt: str, schema: Type[BaseModel], agent_name: str, fallback: FallbackFn
    ) -> LLMResult:
        """Ask the LLM for JSON matching `schema`; run `fallback` if any guardrail blocks."""
        start = time.perf_counter()
        full_prompt = (
            f"{prompt}\n\nRespond with only a JSON object that matches this JSON schema:\n"
            f"{json.dumps(schema.model_json_schema(), separators=(',', ':'))}"
        )
        key = self.cache_key(full_prompt)
        attempts = 0
        tokens = 0
        try:
            if not self._usable(self.model):
                raise _Blocked("disabled")
            cached = await self.store.get_json(key)
            if cached is not None:
                try:
                    data = schema.model_validate(cached).model_dump()
                except Exception:  # noqa: BLE001 - stale/incompatible cache entry: ignore it
                    data = None
                if data is not None:
                    await self._cache_hit()
                    LLM_CALLS.labels(agent_name, "cache").inc()
                    return LLMResult(data, False, cached=True, model=self.model,
                                     latency_ms=_ms(start))
            await self._check_rate(agent_name)
            last = "error"
            for _ in range(2):  # one retry
                await self._reserve_call()
                attempts += 1
                try:
                    resp = await asyncio.wait_for(self._call_completion(full_prompt), self.timeout_s)
                except asyncio.TimeoutError:
                    last = "timeout"
                    continue
                except Exception as err:  # noqa: BLE001 - provider errors are retried, then fall back
                    log.warning("llm %s: provider error: %s", agent_name, err)
                    last = "error"
                    continue
                used = _tokens(resp)
                tokens += used
                await self._add_tokens(agent_name, used)
                try:
                    data = schema.model_validate(_parse_json(resp.choices[0].message.content)).model_dump()
                except Exception as err:  # noqa: BLE001 - malformed output counts as an error
                    log.warning("llm %s: malformed output: %s", agent_name, err)
                    last = "error"
                    continue
                await self.store.set_json(key, data, self.cache_ttl_s)
                latency = _ms(start)
                LLM_CALLS.labels(agent_name, "ok").inc()
                log.info("llm %s model=%s latency_ms=%.1f tokens=%d attempts=%d",
                         agent_name, self.model, latency, tokens, attempts)
                return LLMResult(data, False, latency_ms=latency, tokens=tokens, model=self.model,
                                 attempts=attempts)
            raise _Blocked(last)
        except _Blocked as blocked:
            data = await _run_fallback(fallback, schema)
            latency = _ms(start)
            LLM_CALLS.labels(agent_name, blocked.reason).inc()
            log.info("llm %s fallback=%s latency_ms=%.1f tokens=%d attempts=%d",
                     agent_name, blocked.reason, latency, tokens, attempts)
            return LLMResult(data, True, blocked.reason, latency_ms=latency, tokens=tokens,
                             model="deterministic-fallback", attempts=attempts)

    async def embed(self, text: str, agent_name: str) -> Tuple[Optional[List[float]], Optional[str]]:
        """Embed text under the same guardrails. Returns (vector, None) or (None, reason)."""
        key = self.cache_key(text, model=self.embedding_model)
        try:
            if not self._usable(self.embedding_model):
                raise _Blocked("disabled")
            cached = await self.store.get_json(key)
            if cached is not None:
                await self._cache_hit()
                return cached, None
            await self._check_rate(f"{agent_name}:embed")
            last = "error"
            for _ in range(2):
                await self._reserve_call()
                try:
                    resp = await asyncio.wait_for(self._call_embedding(text), self.timeout_s)
                except asyncio.TimeoutError:
                    last = "timeout"
                    continue
                except Exception as err:  # noqa: BLE001
                    log.warning("embed %s: provider error: %s", agent_name, err)
                    last = "error"
                    continue
                await self._add_tokens(agent_name, _tokens(resp))
                item = resp.data[0]
                vector = [float(v) for v in (item["embedding"] if isinstance(item, dict) else item.embedding)]
                await self.store.set_json(key, vector, self.cache_ttl_s)
                return vector, None
            raise _Blocked(last)
        except _Blocked as blocked:
            return None, blocked.reason

    async def budget(self) -> Dict[str, Any]:
        return {
            "day": ist_day(self.clock()),
            "timezone": "Asia/Kolkata",
            "enabled": self.enabled,
            "model": self.model,
            "calls_today": await self.store.get_int(self._day_key("calls")),
            "tokens_today": await self.store.get_int(self._day_key("tokens")),
            "cache_hits": await self.store.get_int(self._day_key("cache_hits")),
            "limits": {
                "daily_calls": self.call_limit,
                "daily_tokens": self.token_limit,
                "min_interval_s": self.min_interval_s,
                "timeout_s": self.timeout_s,
            },
            "store": self.store.backend,
        }

    # ---- provider calls ----
    def _key_kwargs(self, model: str) -> Dict[str, Any]:
        return {"api_key": self.api_key} if self.api_key and model.startswith("gemini/") else {}

    async def _call_completion(self, prompt: str) -> Any:
        fn = self._completion or litellm.acompletion
        return await fn(
            model=self.model,
            messages=[{"role": "user", "content": prompt}],
            temperature=0.0,
            timeout=self.timeout_s,
            **self._key_kwargs(self.model),
        )

    async def _call_embedding(self, text: str) -> Any:
        fn = self._embedding or litellm.aembedding
        return await fn(model=self.embedding_model, input=[text], timeout=self.timeout_s,
                        **self._key_kwargs(self.embedding_model))


def _ms(start: float) -> float:
    return round((time.perf_counter() - start) * 1000, 3)


async def _run_fallback(fallback: FallbackFn, schema: Type[BaseModel]) -> Dict[str, Any]:
    out = fallback()
    if inspect.isawaitable(out):
        out = await out
    if isinstance(out, BaseModel):
        out = out.model_dump()
    return schema.model_validate(out).model_dump()


# ---- process-wide client built from settings ----
_client: Optional[LLMClient] = None


async def get_llm() -> LLMClient:
    global _client
    if _client is None:
        _client = LLMClient(
            store=GuardStore(await infra.get_redis()),
            model=settings.LLM_MODEL,
            api_key=settings.GEMINI_API_KEY,
            enabled=settings.LLM_ENABLED,
            timeout_s=settings.LLM_TIMEOUT_S,
            call_limit=settings.LLM_DAILY_CALL_LIMIT,
            token_limit=settings.LLM_DAILY_TOKEN_LIMIT,
            min_interval_s=settings.LLM_MIN_INTERVAL_S,
            cache_ttl_s=settings.LLM_CACHE_TTL_S,
            embedding_model=settings.EMBEDDING_MODEL,
        )
    return _client


def reset_llm() -> None:
    global _client
    _client = None


async def complete_json(prompt: str, schema: Type[BaseModel], agent_name: str, fallback: FallbackFn) -> LLMResult:
    """Module-level convenience: `complete_json` on the shared, settings-driven client."""
    return await (await get_llm()).complete_json(prompt, schema, agent_name, fallback)
