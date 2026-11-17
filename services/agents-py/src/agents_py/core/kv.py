"""Counters, locks and cache backed by Redis, with an in-memory fallback.

Every operation tries Redis first. If Redis is not configured, or a call fails,
the same operation runs against process memory so guardrails keep working
(per instance rather than fleet-wide) instead of failing open.
"""
from __future__ import annotations

import datetime as dt
import json
import logging
import time
from typing import Any, Callable, Dict, Optional, Tuple

log = logging.getLogger("hexago.kv")

IST = dt.timezone(dt.timedelta(hours=5, minutes=30))  # India has no DST


def ist_day(ts: float) -> str:
    """The IST calendar day (YYYY-MM-DD) containing unix time ts."""
    return dt.datetime.fromtimestamp(ts, IST).strftime("%Y-%m-%d")


def next_ist_midnight(ts: float) -> int:
    """Unix time of the next IST midnight after ts."""
    now = dt.datetime.fromtimestamp(ts, IST)
    midnight = (now + dt.timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0)
    return int(midnight.timestamp())


class _Memory:
    def __init__(self, clock: Callable[[], float]):
        self.clock = clock
        self.data: Dict[str, Tuple[Any, Optional[float]]] = {}

    def _get(self, key: str) -> Any:
        item = self.data.get(key)
        if item is None:
            return None
        value, exp = item
        if exp is not None and self.clock() >= exp:
            del self.data[key]
            return None
        return value

    def incr(self, key: str, amount: int, expire_at: Optional[int]) -> int:
        value = int(self._get(key) or 0) + amount
        self.data[key] = (value, float(expire_at) if expire_at else None)
        return value

    def get(self, key: str) -> Any:
        return self._get(key)

    def set(self, key: str, value: Any, ttl_s: Optional[float], nx: bool = False) -> bool:
        if nx and self._get(key) is not None:
            return False
        self.data[key] = (value, self.clock() + ttl_s if ttl_s else None)
        return True


class GuardStore:
    """Async KV used by the LLM guardrails. `redis` is a redis.asyncio client or None."""

    def __init__(self, redis: Any = None, clock: Callable[[], float] = time.time):
        self.redis = redis
        self.clock = clock
        self.memory = _Memory(clock)
        self._warned = False

    @property
    def backend(self) -> str:
        return "redis" if self.redis is not None else "memory"

    def _redis_failed(self, op: str, err: Exception) -> None:
        if not self._warned:
            log.warning("redis %s failed, using in-memory guardrail state: %s", op, err)
            self._warned = True

    async def incr(self, key: str, amount: int = 1, expire_at: Optional[int] = None) -> int:
        if self.redis is not None:
            try:
                value = await self.redis.incrby(key, amount)
                if expire_at:
                    await self.redis.expireat(key, expire_at)
                return int(value)
            except Exception as err:  # noqa: BLE001 - any Redis failure degrades to memory
                self._redis_failed("incr", err)
        return self.memory.incr(key, amount, expire_at)

    async def get_int(self, key: str) -> int:
        if self.redis is not None:
            try:
                return int(await self.redis.get(key) or 0)
            except Exception as err:  # noqa: BLE001
                self._redis_failed("get", err)
        return int(self.memory.get(key) or 0)

    async def set_nx(self, key: str, ttl_s: float) -> bool:
        """Set key if absent, expiring after ttl_s. True if this call set it."""
        if ttl_s <= 0:
            return True
        if self.redis is not None:
            try:
                return bool(await self.redis.set(key, "1", nx=True, px=max(1, int(ttl_s * 1000))))
            except Exception as err:  # noqa: BLE001
                self._redis_failed("set", err)
        return self.memory.set(key, "1", ttl_s, nx=True)

    async def get_json(self, key: str) -> Any:
        if self.redis is not None:
            try:
                raw = await self.redis.get(key)
                return json.loads(raw) if raw else None
            except Exception as err:  # noqa: BLE001
                self._redis_failed("get", err)
        raw = self.memory.get(key)
        return json.loads(raw) if raw else None

    async def set_json(self, key: str, value: Any, ttl_s: int) -> None:
        raw = json.dumps(value)
        if self.redis is not None:
            try:
                await self.redis.set(key, raw, ex=ttl_s)
                return
            except Exception as err:  # noqa: BLE001
                self._redis_failed("set", err)
        self.memory.set(key, raw, ttl_s)
