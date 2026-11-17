"""End-to-end REDIS_URL check for /healthz: connect, PING, pub/sub round-trip.

Mirrors dispatch-go's internal/redisdiag so both services report Redis the same
way. Passwords are never reported: URLs come back as scheme://user:****@host:port/db.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import logging
import secrets
import time
from typing import Any, Callable, Dict, Optional
from urllib.parse import urlsplit

log = logging.getLogger("hexago.redis")


def describe(raw: str) -> Dict[str, Any]:
    """Scheme, host, port, db and a masked URL. Raises ValueError for non-Redis URLs."""
    u = urlsplit(raw)
    if u.scheme not in ("redis", "rediss"):
        raise ValueError(f"REDIS_URL scheme {u.scheme!r}: want redis:// or rediss://")
    host = u.hostname or ""
    port = u.port or 6379
    path = u.path.strip("/")
    db = int(path) if path.isdigit() else 0
    auth = ""
    if u.username or u.password:
        auth = (u.username or "") + (":****" if u.password else "") + "@"
    return {
        "url": f"{u.scheme}://{auth}{host}:{port}/{db}",
        "scheme": u.scheme,
        "host": host,
        "port": port,
        "db": db,
        "tls": u.scheme == "rediss",
        "render_internal_host": host.startswith("red-") and "." not in host,
    }


def mask(raw: str) -> str:
    try:
        return describe(raw)["url"]
    except ValueError:
        return "<invalid REDIS_URL>"


def hint(err: BaseException) -> str:
    """Explain a Redis error in terms of the likely Render setting."""
    e = str(err).lower()
    if any(s in e for s in ("name or service not known", "getaddrinfo", "nodename nor servname", "no such host",
                            "temporary failure in name resolution")):
        return ("DNS lookup failed. An internal Render Key Value URL (redis://red-xxxx:6379) only resolves from "
                "services in the same region and workspace as the Key Value instance; otherwise use its external "
                "rediss:// URL.")
    if "certificate" in e:
        return "TLS certificate not accepted for this host; use the exact hostname from the Key Value's external URL."
    if "ssl" in e or "wrong version number" in e or "tls" in e:
        return "TLS mismatch: rediss:// was used against a plain-text endpoint. Internal Render URLs use redis://."
    if any(s in e for s in ("wrongpass", "authentication required", "invalid password", "noauth",
                            "invalid username-password", "without any password configured")):
        return "Authentication failed: the password in REDIS_URL is wrong or missing. Copy the URL again from the Key Value page."
    if "refused" in e:
        return "Connection refused: the host is reachable but nothing accepts connections on that port (wrong host/port)."
    if "closed by server" in e or "connection reset" in e or "broken pipe" in e:
        return ("Server closed the connection. An external Render Key Value URL needs TLS (rediss://) and this "
                "service's outbound IP in the instance's allow list.")
    if "timeout" in e or "timed out" in e:
        return ("Timed out: the host is not reachable from this service (different region, or external access "
                "blocked by the Key Value allow list).")
    return ""


def _now() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _base(raw: str) -> Dict[str, Any]:
    res: Dict[str, Any] = {"configured": bool(raw), "ok": False, "ping_ms": None, "pubsub_roundtrip_ms": None,
                           "failed_stage": None, "error": None, "hint": None, "checked_at": None}
    if raw:
        try:
            res.update(describe(raw))
        except ValueError:
            pass
    return res


async def check(raw: str, timeout_s: float = 5.0, client: Any = None) -> Dict[str, Any]:
    res = _base(raw)
    res["checked_at"] = _now()
    if not raw:
        return res

    def fail(stage: str, err: BaseException) -> Dict[str, Any]:
        h = hint(err)
        if res.get("tls") and stage == "ping" and "timed out" in h.lower():
            # redis-py hangs in the TLS handshake against a plain-text server instead of erroring.
            h += (" With rediss://, a timeout also happens when the endpoint does not speak TLS "
                  "(internal Render URLs are redis://).")
        res.update(failed_stage=stage, error=f"{type(err).__name__}: {err}", hint=h or None)
        return res

    try:
        describe(raw)
    except ValueError as err:
        return fail("parse", err)
    own = client is None
    if own:
        import redis.asyncio as aioredis

        client = aioredis.from_url(raw, decode_responses=True, socket_timeout=timeout_s,
                                   socket_connect_timeout=timeout_s)
    stage = "ping"
    try:
        start = time.perf_counter()
        await asyncio.wait_for(client.ping(), timeout_s)
        res["ping_ms"] = round((time.perf_counter() - start) * 1000, 3)

        channel = f"hexago:diag:{secrets.token_hex(6)}"
        start = time.perf_counter()
        ps = client.pubsub()
        try:
            stage = "subscribe"
            await asyncio.wait_for(ps.subscribe(channel), timeout_s)
            stage = "publish"
            await asyncio.wait_for(client.publish(channel, "ping"), timeout_s)
            stage = "receive"
            deadline = time.perf_counter() + timeout_s
            while True:
                remaining = deadline - time.perf_counter()
                if remaining <= 0:
                    raise TimeoutError("timed out waiting for the published test message")
                msg = await ps.get_message(ignore_subscribe_messages=True, timeout=min(remaining, 0.5))
                if msg and msg.get("data") == "ping":
                    break
        finally:
            try:
                await ps.aclose()
            except Exception:  # noqa: BLE001
                pass
        res["pubsub_roundtrip_ms"] = round((time.perf_counter() - start) * 1000, 3)
        res["ok"] = True
        return res
    except Exception as err:  # noqa: BLE001 - every failure is reported, not raised
        if isinstance(err, asyncio.TimeoutError) and not str(err):
            err = TimeoutError(f"{stage} timed out after {timeout_s}s")
        return fail(stage, err)
    finally:
        if own:
            try:
                await client.aclose()
            except Exception:  # noqa: BLE001
                pass


class RedisMonitor:
    """Re-checks a URL periodically so /healthz never blocks on Redis."""

    def __init__(self, url: str, interval_s: float = 60.0, timeout_s: float = 5.0,
                 client_factory: Optional[Callable[[], Any]] = None):
        self.url = url
        self.interval_s = interval_s
        self.timeout_s = timeout_s
        self.client_factory = client_factory
        self._last: Optional[Dict[str, Any]] = None
        self._last_ok: Optional[bool] = None

    def last(self) -> Dict[str, Any]:
        return dict(self._last) if self._last else _base(self.url)

    async def check_now(self) -> Dict[str, Any]:
        client = self.client_factory() if self.client_factory else None
        res = await check(self.url, self.timeout_s, client=client)
        if res["configured"] and res["ok"] != self._last_ok:
            if res["ok"]:
                log.info("redis check: %s ok (ping %.1f ms, pub/sub %.1f ms)", res["url"], res["ping_ms"],
                         res["pubsub_roundtrip_ms"])
            else:
                log.error("redis check: %s failed at %s: %s. %s", res.get("url", "?"), res["failed_stage"],
                          res["error"], res["hint"] or "")
        self._last, self._last_ok = res, res["ok"]
        return res

    async def run(self) -> None:
        while True:
            await self.check_now()
            await asyncio.sleep(self.interval_s)
