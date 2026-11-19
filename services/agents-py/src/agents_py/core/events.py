"""Agent audit trail: `agent_events` rows + live Ops feed frames.

`record_event` writes one row to the `agent_events` table that dispatch-go
created (creating it if missing) and publishes the Ops feed frame

    {"topic":"ops","data":{"type":"agent_event","agent","decision","reasoning",
                           "confidence","latency_ms","used_fallback","ts", ...optional}}

the exact shape of dispatch-go's ``stream.AgentEvent`` that the Flutter Ops
Command Center parses. The frame goes to two Redis channels:

* ``ops`` - the plain frame, for any service listening for agent events;
* ``hexago:stream`` - ``"ops\\n" + frame``, the gateway broker's wire format,
  so dispatch-go relays it to every ``/v1/stream`` subscriber of ``ops``.

Every event is also kept in a 500-entry in-memory ring, which serves reads
when there is no DATABASE_URL (or Postgres fails), so the service never crashes.
"""
from __future__ import annotations

import collections
import datetime as dt
import json
import logging
import re
from typing import Any, Callable, Deque, Dict, List, Optional

from src.agents_py.core import infra

log = logging.getLogger("hexago.events")

OPS_CHANNEL = "ops"
GATEWAY_CHANNEL = "hexago:stream"  # dispatch-go stream.RedisChannel
RING_SIZE = 500

_DDL = """
CREATE TABLE IF NOT EXISTS agent_events (
    id             BIGSERIAL PRIMARY KEY,
    agent          TEXT NOT NULL,
    event_type     TEXT NOT NULL,
    ride_id        TEXT,
    input          JSONB NOT NULL DEFAULT '{}'::jsonb,
    decision       JSONB NOT NULL DEFAULT '{}'::jsonb,
    confidence     DOUBLE PRECISION,
    fallback_used  BOOLEAN NOT NULL DEFAULT false,
    token_cost     DOUBLE PRECISION NOT NULL DEFAULT 0,
    latency_ms     DOUBLE PRECISION,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS agent_events_created_idx ON agent_events (created_at DESC);
CREATE INDEX IF NOT EXISTS agent_events_agent_idx ON agent_events (agent, created_at DESC);
CREATE OR REPLACE FUNCTION agent_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'agent_events is append-only';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS agent_events_no_mutation ON agent_events;
CREATE TRIGGER agent_events_no_mutation
    BEFORE UPDATE OR DELETE ON agent_events
    FOR EACH ROW EXECUTE FUNCTION agent_events_append_only();
"""

_COLUMNS = ("id, agent, event_type, ride_id, input, decision, confidence, fallback_used, "
            "latency_ms, created_at")


def agent_id(name: str) -> str:
    """'Legal & Jurisdiction' -> 'legal_jurisdiction' (dispatch-go uses snake_case ids)."""
    return re.sub(r"[^a-z0-9]+", "_", name.lower()).strip("_")


def _ts(t: dt.datetime) -> str:
    return t.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")  # Go time.RFC3339


def ops_payload(agent: str, decision: str, reasoning: str, confidence: float, latency_ms: float,
                used_fallback: bool, ts: dt.datetime, *, kind: str = "", ride_id: str = "",
                driver_id: str = "", fallback_reason: Optional[str] = None) -> Dict[str, Any]:
    """The `data` object of an ops agent_event frame, keyed like dispatch-go's AgentEvent."""
    p: Dict[str, Any] = {
        "type": "agent_event",
        "agent": agent,
        "decision": decision,
        "reasoning": reasoning,
        "confidence": round(float(confidence or 0), 3),
        "latency_ms": round(float(latency_ms or 0), 4),
        "used_fallback": bool(used_fallback),
        "ts": _ts(ts),
    }
    for k, v in (("kind", kind), ("ride_id", ride_id), ("driver_id", driver_id),
                 ("fallback_reason", fallback_reason)):
        if v:
            p[k] = v
    return p


def encode_frame(payload: Dict[str, Any]) -> str:
    return json.dumps({"topic": OPS_CHANNEL, "data": payload}, separators=(",", ":"), ensure_ascii=False)


def _loads(v: Any) -> Any:
    if isinstance(v, (str, bytes)):
        try:
            return json.loads(v)
        except ValueError:
            return v
    return v


class EventStore:
    def __init__(self, pool: Any = None, redis: Any = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)):
        self.pool = pool
        self.redis = redis
        self.clock = clock
        self.ring: Deque[Dict[str, Any]] = collections.deque(maxlen=RING_SIZE)
        self._schema_ready = False
        self._seq = 0

    @property
    def backend(self) -> str:
        return "postgres" if self.pool is not None else "memory"

    async def _ensure_schema(self) -> None:
        if self._schema_ready:
            return
        if await self.pool.fetchval("SELECT to_regclass('public.agent_events')") is None:
            log.info("agent_events missing; creating it")
            await self.pool.execute(_DDL)
        self._schema_ready = True

    async def record_event(self, agent_name: str, input_summary: str, decision: str, reasoning: str,
                           confidence: float, latency_ms: float, used_fallback: bool,
                           fallback_reason: Optional[str] = None,
                           metadata: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        metadata = dict(metadata or {})
        now = self.clock()
        name = agent_id(agent_name)
        payload = ops_payload(
            name, decision, reasoning, confidence, latency_ms, used_fallback, now,
            kind=str(metadata.get("kind", "")), ride_id=str(metadata.get("ride_id", "")),
            driver_id=str(metadata.get("driver_id", "")),
            fallback_reason=fallback_reason if used_fallback else None,
        )
        self._seq += 1
        event = {**payload, "id": self._seq, "input_summary": input_summary, "metadata": metadata,
                 "_at": now}
        self.ring.appendleft(event)

        if self.pool is not None:
            try:
                await self._ensure_schema()
                await self.pool.execute(
                    "INSERT INTO agent_events (agent, event_type, ride_id, input, decision, confidence, "
                    "fallback_used, token_cost, latency_ms, created_at) "
                    "VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9, $10)",
                    name, str(metadata.get("kind") or "decision"), metadata.get("ride_id") or None,
                    json.dumps({"summary": input_summary}, default=str),
                    json.dumps({"decision": decision, "reasoning": reasoning,
                                "fallback_reason": fallback_reason, "metadata": metadata}, default=str),
                    float(confidence or 0), bool(used_fallback), float(metadata.get("tokens") or 0),
                    float(latency_ms or 0), now,
                )
            except Exception as err:  # noqa: BLE001 - the audit write must never break an agent
                log.warning("write agent_events failed (kept in memory): %s", err)

        if self.redis is not None:
            frame = encode_frame(payload)
            try:
                await self.redis.publish(OPS_CHANNEL, frame)
                await self.redis.publish(GATEWAY_CHANNEL, f"{OPS_CHANNEL}\n{frame}")
            except Exception as err:  # noqa: BLE001
                log.warning("publish ops event failed: %s", err)
        return _public(event)

    async def recent(self, limit: int = 50, agent: Optional[str] = None) -> List[Dict[str, Any]]:
        limit = max(1, min(int(limit), RING_SIZE))
        wanted = agent_id(agent) if agent else None
        if self.pool is not None:
            try:
                await self._ensure_schema()
                if wanted:
                    rows = await self.pool.fetch(
                        f"SELECT {_COLUMNS} FROM agent_events WHERE agent = $1 ORDER BY id DESC LIMIT $2",
                        wanted, limit)
                else:
                    rows = await self.pool.fetch(
                        f"SELECT {_COLUMNS} FROM agent_events ORDER BY id DESC LIMIT $1", limit)
                return [_from_row(r) for r in rows]
            except Exception as err:  # noqa: BLE001
                log.warning("read agent_events failed (serving memory): %s", err)
        out = [e for e in self.ring if wanted is None or e["agent"] == wanted]
        return [_public(e) for e in out[:limit]]

    async def agent_stats(self, since: dt.datetime) -> Dict[str, Dict[str, Any]]:
        """Per agent id: decisions since `since`, how many used a fallback, last decision ever, and the
        model named by its latest event that recorded one (LLM or rule model) in the day before `since`."""
        stats: Dict[str, Dict[str, Any]] = {}

        def entry(name: str) -> Dict[str, Any]:
            return stats.setdefault(agent_id(name), {"count": 0, "fallbacks": 0, "last": None, "model": None})

        if self.pool is not None:
            try:
                await self._ensure_schema()
                for r in await self.pool.fetch(
                        "SELECT agent, count(*) AS n, count(*) FILTER (WHERE fallback_used) AS fallbacks "
                        "FROM agent_events WHERE created_at >= $1 GROUP BY agent", since):
                    e = entry(r["agent"])
                    e["count"] += int(r["n"])
                    e["fallbacks"] += int(r["fallbacks"])
                for r in await self.pool.fetch(
                        f"SELECT DISTINCT ON (agent) {_COLUMNS} FROM agent_events "
                        "ORDER BY agent, created_at DESC, id DESC"):
                    entry(r["agent"])["last"] = _from_row(r)
                for r in await self.pool.fetch(
                        "SELECT DISTINCT ON (agent) agent, decision->'metadata'->>'model' AS model FROM agent_events "
                        "WHERE created_at >= $1 AND decision->'metadata'->>'model' IS NOT NULL "
                        "ORDER BY agent, created_at DESC", since - dt.timedelta(days=1)):
                    if r.get("model"):
                        entry(r["agent"])["model"] = r["model"]
                return stats
            except Exception as err:  # noqa: BLE001
                log.warning("agent stats query failed (serving memory): %s", err)
                stats.clear()
        for e in self.ring:  # newest first
            s = entry(e["agent"])
            if s["last"] is None:
                s["last"] = _public(e)
            if s["model"] is None and (e.get("metadata") or {}).get("model"):
                s["model"] = e["metadata"]["model"]
            if e["_at"] >= since:
                s["count"] += 1
                s["fallbacks"] += int(e["used_fallback"])
        return stats


def _public(e: Dict[str, Any]) -> Dict[str, Any]:
    return {k: v for k, v in e.items() if not k.startswith("_")}


def _rings(n: Any) -> str:
    return f"{n} H3 ring" + ("" if n == 1 else "s")


def _describe_structured(event_type: str, ride_id: str, d: Dict[str, Any]) -> tuple:
    """One-line text for dispatch-go's structured match / no_match rows (as its stream shows them)."""
    try:
        if event_type == "match" and "driver_id" in d:
            sc = d.get("score") or {}
            decision = (f"matched ride {ride_id} to {d['driver_id']} ({d.get('matched_tier', '?')}), "
                        f"pickup ETA {float(sc.get('eta_seconds', 0)) / 60:.1f} min")
            if d.get("upgraded"):
                decision += f", upgraded from {d.get('requested_tier', '?')}"
            reasoning = (f"Best of {d.get('candidates_considered', 0)} candidates within "
                         f"{_rings(d.get('rings_searched', 0))} (~{float(d.get('search_radius_km', 0)):.1f} km): "
                         f"{float(sc.get('distance_km', 0)):.2f} km away, score {float(sc.get('total', 0)):.2f}.")
            return decision, reasoning
        if event_type == "no_match":
            return (f"no driver available for ride {ride_id} ({d.get('requested_tier', '?')}); request left open",
                    f"Searched {_rings(d.get('rings_searched', 0))} "
                    f"(~{float(d.get('search_radius_km', 0)):.1f} km); {d.get('candidates_found', 0)} candidates found.")
    except (TypeError, ValueError):
        pass
    return event_type or "decision", json.dumps(d)[:300]


def _from_row(r: Any) -> Dict[str, Any]:
    dec = _loads(r["decision"])
    inp = _loads(r["input"])
    if isinstance(dec, dict) and "decision" in dec:
        decision, reasoning = str(dec.get("decision") or ""), str(dec.get("reasoning") or "")
        fallback_reason = dec.get("fallback_reason")
        metadata = dec.get("metadata") or {}
    elif isinstance(dec, dict):
        decision, reasoning = _describe_structured(r["event_type"] or "", r["ride_id"] or "", dec)
        fallback_reason, metadata = None, dec
    else:
        decision = str(dec)
        reasoning, fallback_reason, metadata = "", None, {}
    payload = ops_payload(
        r["agent"], decision, reasoning, r["confidence"] or 0, r["latency_ms"] or 0, r["fallback_used"],
        r["created_at"], kind=r["event_type"] or "", ride_id=r["ride_id"] or "",
        fallback_reason=fallback_reason if r["fallback_used"] else None,
    )
    summary = inp.get("summary", "") if isinstance(inp, dict) else ""
    return {**payload, "id": r["id"], "input_summary": summary, "metadata": metadata}


# ---- process-wide store ----
_store: Optional[EventStore] = None


async def get_event_store() -> EventStore:
    global _store
    if _store is None:
        _store = EventStore(pool=await infra.get_pool(), redis=await infra.get_redis())
    else:  # pick up Redis/Postgres that connected after the store was built
        if _store.redis is None:
            _store.redis = await infra.get_redis()
        if _store.pool is None:
            _store.pool = await infra.get_pool()
    return _store


def reset_event_store() -> None:
    global _store
    _store = None


async def record_event(agent_name: str, input_summary: str, decision: str, reasoning: str,
                       confidence: float, latency_ms: float, used_fallback: bool,
                       fallback_reason: Optional[str] = None,
                       metadata: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    """Record one agent decision (DB row + Ops feed frame). Never raises on infra failure."""
    store = await get_event_store()
    return await store.record_event(agent_name, input_summary, decision, reasoning, confidence,
                                    latency_ms, used_fallback, fallback_reason, metadata)
