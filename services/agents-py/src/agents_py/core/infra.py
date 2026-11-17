"""Lazy, failure-tolerant connections to Redis and Postgres.

Both are optional: an empty REDIS_URL / DATABASE_URL, or a server that cannot be
reached, yields None and callers fall back to in-memory state. A failed connect
is retried at most once per RETRY_S so a down dependency is not hammered.
"""
from __future__ import annotations

import logging
import time
from typing import Any, Optional

from src.agents_py.config import settings
from src.agents_py.core import redisdiag

log = logging.getLogger("hexago.infra")

RETRY_S = 60.0

_redis: Any = None
_pool: Any = None
_redis_failed_at: Optional[float] = None
_pool_failed_at: Optional[float] = None


def _may_retry(failed_at: Optional[float]) -> bool:
    return failed_at is None or time.monotonic() - failed_at >= RETRY_S


async def get_redis() -> Any:
    """A connected redis.asyncio client (decode_responses=True), or None."""
    global _redis, _redis_failed_at
    if _redis is not None or not settings.REDIS_URL or not _may_retry(_redis_failed_at):
        return _redis
    try:
        import redis.asyncio as aioredis

        client = aioredis.from_url(settings.REDIS_URL, decode_responses=True,
                                   socket_timeout=2, socket_connect_timeout=2)
        await client.ping()
        _redis, _redis_failed_at = client, None
        log.info("redis connected: %s", redisdiag.mask(settings.REDIS_URL))
    except Exception as err:  # noqa: BLE001 - any failure means "run without Redis"
        log.error("redis %s unavailable, using in-memory state (retry in %ds): %s: %s. %s",
                  redisdiag.mask(settings.REDIS_URL), RETRY_S, type(err).__name__, err, redisdiag.hint(err))
        _redis_failed_at = time.monotonic()
    return _redis


_monitor: Optional[redisdiag.RedisMonitor] = None


def redis_monitor() -> redisdiag.RedisMonitor:
    """The process-wide REDIS_URL monitor reported on /healthz."""
    global _monitor
    if _monitor is None or _monitor.url != settings.REDIS_URL:
        _monitor = redisdiag.RedisMonitor(settings.REDIS_URL)
    return _monitor


async def get_pool() -> Any:
    """An asyncpg pool, or None."""
    global _pool, _pool_failed_at
    if _pool is not None or not settings.DATABASE_URL or not _may_retry(_pool_failed_at):
        return _pool
    try:
        import asyncpg

        _pool = await asyncpg.create_pool(settings.DATABASE_URL, min_size=1, max_size=5, timeout=5,
                                          command_timeout=5)
        _pool_failed_at = None
    except Exception as err:  # noqa: BLE001
        log.warning("postgres unavailable, using in-memory state: %s", err)
        _pool_failed_at = time.monotonic()
    return _pool


def override(redis: Any = None, pool: Any = None) -> None:
    """Inject clients (tests)."""
    global _redis, _pool
    _redis, _pool = redis, pool


def reset() -> None:
    global _redis, _pool, _redis_failed_at, _pool_failed_at, _monitor
    _redis = _pool = _redis_failed_at = _pool_failed_at = _monitor = None


async def close() -> None:
    for client, closer in ((_redis, "aclose"), (_pool, "close")):
        if client is not None:
            try:
                await getattr(client, closer)()
            except Exception:  # noqa: BLE001
                pass
    reset()


async def status() -> dict:
    """Component status for /healthz."""
    redis = await get_redis()
    pool = await get_pool()
    return {
        "redis": "up" if redis is not None else ("down" if settings.REDIS_URL else "not_configured"),
        "postgres": "up" if pool is not None else ("down" if settings.DATABASE_URL else "not_configured"),
    }
