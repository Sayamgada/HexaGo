"""Counters, locks and cache backed by Redis, with an in-memory fallback.

Every operation tries Redis first. If Redis is not configured, or a call fails,
the same operation runs against process memory so guardrails keep working
(per instance rather than fleet-wide) instead of failing open.

All time reads go through the injected ``clock`` (``time.time`` in production).
Redis expiry runs on the server's own wall clock, so this module never hands
Redis an absolute timestamp: ``expire_at`` is turned into a TTL relative to
``clock()``, and keys whose freshness matters (min-interval locks, cached
responses) also store their own deadline, which is checked against ``clock()``.
The Redis TTL is then only garbage collection.
"""
from __future__ import annotations

import datetime as dt
import json
import logging
import math
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


def _ttl_until(expire_at: float, now: float) -> int:
    """Whole seconds from now until expire_at, at least 1 (Redis rejects 0)."""
    return max(1, math.ceil(expire_at - now))


def _deadline(raw: Any) -> float:
    """Deadline stored in a lock key; unparseable (legacy) values count as expired."""
    try:
        return float(raw)
    except (TypeError, ValueError):
        return 0.0


_ENVELOPE = "_exp"  # cached JSON is stored as {"_exp": deadline, "v": value}


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
                    # Never EXPIREAT: Redis would compare it with its own clock.
                    await self.redis.expire(key, _ttl_until(expire_at, self.clock()))
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
                return await self._redis_set_nx(key, ttl_s)
            except Exception as err:  # noqa: BLE001
                self._redis_failed("set", err)
        return self.memory.set(key, "1", ttl_s, nx=True)

    async def _redis_set_nx(self, key: str, ttl_s: float) -> bool:
        """SET NX whose expiry follows clock(): the value is the lock's deadline."""
        now = self.clock()
        deadline = repr(now + ttl_s)
        px = max(1, int(ttl_s * 1000))
        if await self.redis.set(key, deadline, nx=True, px=px):
            return True
        held = await self.redis.get(key)
        if held is not None and _deadline(held) > now:
            return False
        # Expired by our clock but not yet by Redis's: take it over atomically.
        # If another caller swapped it first, the previous value differs and it won.
        previous = await self.redis.set(key, deadline, px=px, get=True)
        return previous is None or previous == held

    async def get_json(self, key: str) -> Any:
        if self.redis is not None:
            try:
                return self._unwrap(await self.redis.get(key))
            except Exception as err:  # noqa: BLE001
                self._redis_failed("get", err)
        return self._unwrap(self.memory.get(key))

    def _unwrap(self, raw: Any) -> Any:
        if not raw:
            return None
        item = json.loads(raw)
        # Entries without an envelope predate clock-checked expiry: treat as a miss.
        if not isinstance(item, dict) or _ENVELOPE not in item:
            return None
        return item["v"] if float(item[_ENVELOPE]) > self.clock() else None

    async def set_json(self, key: str, value: Any, ttl_s: int) -> None:
        raw = json.dumps({_ENVELOPE: self.clock() + ttl_s, "v": value})
        if self.redis is not None:
            try:
                await self.redis.set(key, raw, ex=ttl_s)
                return
            except Exception as err:  # noqa: BLE001
                self._redis_failed("set", err)
        self.memory.set(key, raw, ttl_s)
