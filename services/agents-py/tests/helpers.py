"""Test doubles: fake clock, scripted LLM provider, fake asyncpg pool."""
import json
import re
from types import SimpleNamespace
from typing import Any, Callable, List, Optional

from src.agents_py.core.kv import GuardStore
from src.agents_py.core.llm import LLMClient


class FakeClock:
    def __init__(self, t: float = 1_800_000_000.0):
        self.t = t

    def __call__(self) -> float:
        return self.t

    def advance(self, s: float) -> None:
        self.t += s


class FakeSleep:
    """Async stand-in for asyncio.sleep: records waits instead of blocking."""

    def __init__(self):
        self.waits: List[float] = []

    async def __call__(self, s: float) -> None:
        self.waits.append(s)


def llm_response(content: Any, tokens: int = 50):
    text = content if isinstance(content, str) else json.dumps(content)
    return SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content=text))],
        usage=SimpleNamespace(total_tokens=tokens),
    )


def embedding_response(vector: List[float], tokens: int = 5):
    return SimpleNamespace(data=[{"embedding": vector}], usage=SimpleNamespace(total_tokens=tokens))


class ScriptedProvider:
    """Async stand-in for litellm.acompletion / aembedding.

    Each call pops the next scripted item: an Exception is raised, a callable is
    called (and awaited if needed) with the kwargs, anything else is returned.
    """

    def __init__(self, script: Optional[List[Any]] = None):
        self.script = list(script or [])
        self.calls: List[dict] = []

    async def __call__(self, **kwargs):
        self.calls.append(kwargs)
        item = self.script.pop(0) if self.script else llm_response({})
        if isinstance(item, BaseException):
            raise item
        if callable(item):
            res = item(**kwargs)
            if hasattr(res, "__await__"):
                res = await res
            return res
        return item


def make_llm(redis=None, clock: Optional[Callable[[], float]] = None, completion=None, embedding=None,
             **overrides) -> LLMClient:
    cfg = dict(
        model="gemini/gemini-2.0-flash",
        api_key="test-key",
        enabled=True,
        timeout_s=0.2,
        call_limit=300,
        token_limit=200_000,
        min_interval_s=20,
        cache_ttl_s=600,
        embedding_model="gemini/text-embedding-004",
    )
    cfg.update(overrides)
    clock = clock or FakeClock()
    cfg.setdefault("sleep", FakeSleep())
    return LLMClient(store=GuardStore(redis, clock=clock), clock=clock,
                     completion=completion or ScriptedProvider(), embedding=embedding or ScriptedProvider(),
                     **cfg)


class FakePool:
    """Minimal asyncpg-pool double: records SQL and answers from a handler."""

    def __init__(self, handler: Optional[Callable[[str, tuple], Any]] = None, fail: bool = False):
        self.handler = handler or (lambda sql, args: None)
        self.fail = fail
        self.log: List[tuple] = []

    def _run(self, kind, sql, args):
        self.log.append((kind, re.sub(r"\s+", " ", sql).strip(), args))
        if self.fail:
            raise ConnectionError("db down")
        return self.handler(sql, args)

    def sql(self) -> List[str]:
        return [s for _, s, _ in self.log]

    async def execute(self, sql, *args):
        self._run("execute", sql, args)
        return "OK"

    async def fetch(self, sql, *args):
        return self._run("fetch", sql, args) or []

    async def fetchrow(self, sql, *args):
        return self._run("fetchrow", sql, args)

    async def fetchval(self, sql, *args):
        return self._run("fetchval", sql, args)

    async def close(self):
        pass
