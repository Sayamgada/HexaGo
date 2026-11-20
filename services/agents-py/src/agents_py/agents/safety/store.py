"""Incidents, their timelines and rider check-ins.

Memory is the working set (it serves every read while the process lives);
each mutation is also written through to Postgres - tables ``incidents``,
``incident_timeline`` and ``safety_checkins`` from dispatch-go's migration
``0100_safety_incidents.sql`` (created here too if missing). After a restart,
reads fall back to Postgres. A database failure is logged and never breaks
the sentinel.

Closing an incident needs an approved `Confirmation` from the Ops Supervisor:
`close_incident` refuses anything else, so the Safety Sentinel cannot close
its own incidents.
"""
from __future__ import annotations

import copy
import datetime as dt
import json
import logging
import secrets
from typing import Any, Callable, Dict, List, Optional

from src.agents_py.agents.safety.decision import Confirmation

log = logging.getLogger("hexago.safety.store")

DDL = """
CREATE TABLE IF NOT EXISTS incidents (
    id            TEXT PRIMARY KEY,
    ride_id       TEXT,
    driver_id     TEXT,
    rider_id      TEXT,
    tier          TEXT,
    signal_id     TEXT,
    severity      DOUBLE PRECISION NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    decision      TEXT NOT NULL,
    reason        TEXT,
    reasoning     TEXT,
    data          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ,
    resolved_by   TEXT,
    resolution    TEXT
);
CREATE INDEX IF NOT EXISTS incidents_created_idx ON incidents (created_at DESC);
CREATE INDEX IF NOT EXISTS incidents_driver_idx ON incidents (driver_id, created_at DESC);
CREATE TABLE IF NOT EXISTS incident_timeline (
    id           TEXT PRIMARY KEY,
    incident_id  TEXT NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    step         TEXT NOT NULL,
    actor        TEXT NOT NULL,
    details      JSONB NOT NULL DEFAULT '{}'::jsonb,
    at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS incident_timeline_incident_idx ON incident_timeline (incident_id, at);
CREATE TABLE IF NOT EXISTS safety_checkins (
    id           TEXT PRIMARY KEY,
    incident_id  TEXT NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    ride_id      TEXT,
    question     TEXT NOT NULL,
    status       TEXT NOT NULL,
    timeout_s    DOUBLE PRECISION NOT NULL,
    data         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline_at  TIMESTAMPTZ NOT NULL,
    answered_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS safety_checkins_incident_idx ON safety_checkins (incident_id);
"""

# Incident columns kept outside the `data` JSON blob.
_COLS = ("id", "ride_id", "driver_id", "rider_id", "tier", "signal_id", "severity", "status", "decision",
         "reason", "reasoning", "created_at", "updated_at", "resolved_at", "resolved_by", "resolution")
_CHECKIN_COLS = ("id", "incident_id", "ride_id", "question", "status", "timeout_s", "created_at",
                 "deadline_at", "answered_at")


class ResolutionRefused(PermissionError):
    """Raised when an incident is closed without an approved supervisor confirmation."""


def new_id(prefix: str) -> str:
    return prefix + secrets.token_hex(6)


def _iso(v: Any) -> Any:
    if isinstance(v, dt.datetime):
        return v.astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    return v


def public(obj: Any) -> Any:
    """JSON-safe deep copy (datetimes as ISO 8601 UTC)."""
    if isinstance(obj, dict):
        return {k: public(v) for k, v in obj.items() if not str(k).startswith("_")}
    if isinstance(obj, (list, tuple)):
        return [public(v) for v in obj]
    return _iso(obj)


def _loads(v: Any) -> Any:
    if isinstance(v, (str, bytes)):
        try:
            return json.loads(v)
        except ValueError:
            return {}
    return v if v is not None else {}


class SafetyStore:
    def __init__(self, pool: Any = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)):
        self.pool = pool
        self.clock = clock
        self.incidents: Dict[str, Dict[str, Any]] = {}
        self.checkins: Dict[str, Dict[str, Any]] = {}
        self.escalations: Dict[str, List[float]] = {}  # driver -> epoch seconds
        self.suppressed: List[Dict[str, Any]] = []
        self._schema_ready = False

    @property
    def backend(self) -> str:
        return "postgres" if self.pool is not None else "memory"

    # ---- database plumbing ----

    async def _ensure_schema(self) -> None:
        if self._schema_ready:
            return
        if await self.pool.fetchval("SELECT to_regclass('public.incident_timeline')") is None:
            log.info("safety tables missing; creating them")
            await self.pool.execute(DDL)
        self._schema_ready = True

    async def _exec(self, sql: str, *args: Any) -> None:
        if self.pool is None:
            return
        try:
            await self._ensure_schema()
            await self.pool.execute(sql, *args)
        except Exception as err:  # noqa: BLE001 - persistence must never break the sentinel
            log.warning("safety store write failed (kept in memory): %s", err)

    async def _fetch(self, sql: str, *args: Any) -> Optional[List[Any]]:
        if self.pool is None:
            return None
        try:
            await self._ensure_schema()
            return list(await self.pool.fetch(sql, *args))
        except Exception as err:  # noqa: BLE001
            log.warning("safety store read failed (serving memory): %s", err)
            return None

    async def _save_incident(self, inc: Dict[str, Any]) -> None:
        data = {k: v for k, v in inc.items() if k not in _COLS and k != "timeline"}
        await self._exec(
            "INSERT INTO incidents (id, ride_id, driver_id, rider_id, tier, signal_id, severity, status, decision, "
            "reason, reasoning, data, created_at, updated_at, resolved_at, resolved_by, resolution) "
            "VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$14,$15,$16,$17) "
            "ON CONFLICT (id) DO UPDATE SET severity=EXCLUDED.severity, status=EXCLUDED.status, "
            "decision=EXCLUDED.decision, reason=EXCLUDED.reason, reasoning=EXCLUDED.reasoning, data=EXCLUDED.data, "
            "updated_at=EXCLUDED.updated_at, resolved_at=EXCLUDED.resolved_at, resolved_by=EXCLUDED.resolved_by, "
            "resolution=EXCLUDED.resolution",
            *(inc.get(c) for c in _COLS[:11]), json.dumps(public(data)),
            *(inc.get(c) for c in _COLS[11:]),
        )

    async def _save_checkin(self, c: Dict[str, Any]) -> None:
        data = {k: v for k, v in c.items() if k not in _CHECKIN_COLS}
        await self._exec(
            "INSERT INTO safety_checkins (id, incident_id, ride_id, question, status, timeout_s, data, created_at, "
            "deadline_at, answered_at) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10) "
            "ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, data=EXCLUDED.data, answered_at=EXCLUDED.answered_at",
            c["id"], c["incident_id"], c.get("ride_id"), c["question"], c["status"], float(c["timeout_s"]),
            json.dumps(public(data)), c["created_at"], c["deadline_at"], c.get("answered_at"),
        )

    # ---- incidents ----

    async def create_incident(self, **fields: Any) -> Dict[str, Any]:
        now = self.clock()
        inc: Dict[str, Any] = {
            "id": new_id("inc_"), "severity": 0.0, "status": "open", "decision": "monitor", "reason": "",
            "reasoning": "", "reasoning_source": "template", "triggers": [], "location": None,
            "planned_route": [], "trail": [], "actions": {}, "transcript": [], "checkin_ids": [],
            "created_at": now, "updated_at": now, "resolved_at": None, "resolved_by": None, "resolution": None,
            "timeline": [],
        }
        inc.update(fields)
        self.incidents[inc["id"]] = inc
        await self._save_incident(inc)
        return inc

    async def update_incident(self, incident_id: str, **fields: Any) -> Dict[str, Any]:
        inc = self.incidents[incident_id]
        if "status" in fields and fields["status"] == "resolved":
            raise ResolutionRefused("use close_incident with an Ops Supervisor confirmation")
        inc.update(fields)
        inc["updated_at"] = self.clock()
        await self._save_incident(inc)
        return inc

    async def set_action(self, incident_id: str, name: str, value: Dict[str, Any]) -> None:
        inc = self.incidents[incident_id]
        inc["actions"][name] = value
        inc["updated_at"] = self.clock()
        await self._save_incident(inc)

    async def add_transcript(self, incident_id: str, role: str, text: str, **extra: Any) -> None:
        inc = self.incidents[incident_id]
        inc["transcript"].append({"role": role, "text": text, "at": self.clock(), **extra})
        await self._save_incident(inc)

    async def add_step(self, incident_id: str, step: str, actor: str, **details: Any) -> Dict[str, Any]:
        entry = {"id": new_id("tl_"), "at": self.clock(), "step": step, "actor": actor, "details": details}
        self.incidents[incident_id]["timeline"].append(entry)
        await self._exec(
            "INSERT INTO incident_timeline (id, incident_id, step, actor, details, at) VALUES ($1,$2,$3,$4,$5::jsonb,$6)",
            entry["id"], incident_id, step, actor, json.dumps(public(details)), entry["at"])
        return entry

    async def update_step(self, incident_id: str, step_id: str, **details: Any) -> None:
        for entry in self.incidents[incident_id]["timeline"]:
            if entry["id"] == step_id:
                entry["details"].update(details)
                await self._exec("UPDATE incident_timeline SET details = $2::jsonb WHERE id = $1",
                                 step_id, json.dumps(public(entry["details"])))
                return

    async def close_incident(self, incident_id: str, confirmation: Optional[Confirmation], resolution: str,
                             resolved_by: str) -> Dict[str, Any]:
        """Close an incident. Requires the Ops Supervisor's approved confirmation for this incident."""
        if confirmation is None or not confirmation.valid or confirmation.incident_id != incident_id:
            raise ResolutionRefused("closing an incident requires an approved Ops Supervisor confirmation")
        inc = self.incidents[incident_id]
        now = self.clock()
        inc.update(status="resolved", resolved_at=now, resolved_by=resolved_by, resolution=resolution, updated_at=now)
        await self._save_incident(inc)
        return inc

    async def get_incident(self, incident_id: str) -> Optional[Dict[str, Any]]:
        if incident_id in self.incidents:
            return self.incidents[incident_id]
        rows = await self._fetch("SELECT * FROM incidents WHERE id = $1", incident_id)
        if not rows:
            return None
        inc = self._from_row(rows[0])
        steps = await self._fetch(
            "SELECT id, step, actor, details, at FROM incident_timeline WHERE incident_id = $1 ORDER BY at, id",
            incident_id) or []
        inc["timeline"] = [{"id": s["id"], "step": s["step"], "actor": s["actor"], "details": _loads(s["details"]),
                            "at": s["at"]} for s in steps]
        # Cache it: after a restart the incident lives only in Postgres, and every later step
        # (supervisor review, unfreeze, close) works on the in-memory copy.
        self.incidents[incident_id] = inc
        return inc

    async def list_incidents(self, limit: int = 50) -> List[Dict[str, Any]]:
        limit = max(1, min(int(limit), 200))
        rows = await self._fetch("SELECT * FROM incidents ORDER BY created_at DESC LIMIT $1", limit)
        merged = {r["id"]: self._from_row(r) for r in rows or []}
        merged.update(self.incidents)  # memory is fresher
        out = sorted(merged.values(), key=lambda i: i["created_at"], reverse=True)
        return out[:limit]

    async def incidents_for_driver(self, driver_id: str, limit: int = 20) -> List[Dict[str, Any]]:
        mem = [i for i in self.incidents.values() if i.get("driver_id") == driver_id]
        if not mem:
            rows = await self._fetch(
                "SELECT * FROM incidents WHERE driver_id = $1 ORDER BY created_at DESC LIMIT $2", driver_id, limit)
            mem = [self._from_row(r) for r in rows or []]
        return sorted(mem, key=lambda i: i["created_at"], reverse=True)[:limit]

    @staticmethod
    def _from_row(r: Any) -> Dict[str, Any]:
        inc = dict(_loads(r["data"]))
        inc.setdefault("actions", {})
        inc.setdefault("transcript", [])
        inc.setdefault("checkin_ids", [])
        for c in _COLS:
            inc[c] = r[c]
        inc.setdefault("timeline", [])
        return inc

    # ---- check-ins ----

    async def create_checkin(self, incident_id: str, ride_id: str, question: str, timeout_s: float,
                             **extra: Any) -> Dict[str, Any]:
        now = self.clock()
        c = {"id": new_id("chk_"), "incident_id": incident_id, "ride_id": ride_id, "question": question,
             "status": "pending", "timeout_s": float(timeout_s), "created_at": now,
             "deadline_at": now + dt.timedelta(seconds=timeout_s), "answered_at": None, "transcript": None,
             "classification": None, **extra}
        self.checkins[c["id"]] = c
        self.incidents[incident_id]["checkin_ids"].append(c["id"])
        await self._save_checkin(c)
        return c

    async def update_checkin(self, checkin_id: str, **fields: Any) -> Dict[str, Any]:
        c = self.checkins[checkin_id]
        c.update(fields)
        await self._save_checkin(c)
        return c

    async def get_checkin(self, checkin_id: str) -> Optional[Dict[str, Any]]:
        if checkin_id in self.checkins:
            return self.checkins[checkin_id]
        rows = await self._fetch("SELECT * FROM safety_checkins WHERE id = $1", checkin_id)
        if not rows:
            return None
        r = rows[0]
        c = dict(_loads(r["data"]))
        for k in _CHECKIN_COLS:
            c[k] = r[k]
        return c

    def checkins_for(self, incident_id: str) -> List[Dict[str, Any]]:
        inc = self.incidents.get(incident_id) or {}
        return [self.checkins[c] for c in inc.get("checkin_ids", []) if c in self.checkins]

    def pending_checkins(self) -> List[Dict[str, Any]]:
        out = [c for c in self.checkins.values() if c["status"] == "pending"]
        return sorted(out, key=lambda c: c["created_at"], reverse=True)

    # ---- rate limit + suppressed false positives ----

    def escalation_times(self, driver_id: str) -> List[float]:
        return list(self.escalations.get(driver_id, []))

    def record_escalation(self, driver_id: str, at: float) -> None:
        times = [t for t in self.escalations.get(driver_id, []) if at - t < 24 * 3600]
        times.append(at)
        self.escalations[driver_id] = times

    def record_suppressed(self, entry: Dict[str, Any]) -> None:
        self.suppressed.insert(0, {**entry, "at": self.clock()})
        del self.suppressed[100:]

    def snapshot(self, inc: Dict[str, Any]) -> Dict[str, Any]:
        return public(copy.deepcopy(inc))
