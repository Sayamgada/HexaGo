"""Jurisdiction rules and the rule-change audit log.

Memory is the working set, seeded with Bengaluru, Mumbai and Delhi. With a
Postgres pool every write goes through to tables ``jurisdictions`` and
``legal_audit`` (created here if missing; seeds use ON CONFLICT DO NOTHING so a
restart never resets live rules) and reads come from Postgres, so rule changes
survive restarts. A database failure is logged and the memory copy is served.
"""
from __future__ import annotations

import copy
import datetime as dt
import json
import logging
import secrets
from typing import Any, Callable, Dict, List, Optional

from src.agents_py.agents.legal.legal_rules import seed_jurisdictions

log = logging.getLogger("hexago.legal.store")

DDL = """
CREATE TABLE IF NOT EXISTS jurisdictions (
    city        TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    state       TEXT,
    authority   TEXT,
    rules       JSONB NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    updated_at  TIMESTAMPTZ,
    updated_by  TEXT
);
CREATE TABLE IF NOT EXISTS legal_audit (
    id          TEXT PRIMARY KEY,
    city        TEXT NOT NULL,
    entry       JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS legal_audit_created_idx ON legal_audit (created_at DESC);
"""

_UPSERT = ("INSERT INTO jurisdictions (city, name, state, authority, rules, version, updated_at, updated_by) "
           "VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8) ON CONFLICT (city) {action}")
AUDIT_KEEP = 500


def _iso(v: Any) -> Any:
    if isinstance(v, dt.datetime):
        return v.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    return v


def _loads(v: Any) -> Any:
    if isinstance(v, (str, bytes)):
        try:
            return json.loads(v)
        except ValueError:
            return {}
    return v if v is not None else {}


class LegalStore:
    def __init__(self, pool: Any = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)):
        self.pool = pool
        self.clock = clock
        self.jurisdictions: Dict[str, Dict[str, Any]] = {}
        for city, j in seed_jurisdictions().items():
            self.jurisdictions[city] = {**j, "version": 1, "updated_at": None, "updated_by": "seed"}
        self.audit_log: List[Dict[str, Any]] = []
        self._schema_ready = False

    @property
    def backend(self) -> str:
        return "postgres" if self.pool is not None else "memory"

    async def _ensure_schema(self) -> None:
        if self._schema_ready:
            return
        if await self.pool.fetchval("SELECT to_regclass('public.legal_audit')") is None:
            log.info("legal tables missing; creating them")
            await self.pool.execute(DDL)
        for j in seed_jurisdictions().values():
            await self.pool.execute(_UPSERT.format(action="DO NOTHING"), j["city"], j["name"], j["state"],
                                    j["authority"], json.dumps(j["rules"]), 1, None, "seed")
        self._schema_ready = True

    async def _pull(self) -> None:
        """Refresh the working set from Postgres (no-op without a pool; memory on failure)."""
        if self.pool is None:
            return
        try:
            await self._ensure_schema()
            rows = await self.pool.fetch(
                "SELECT city, name, state, authority, rules, version, updated_at, updated_by FROM jurisdictions")
        except Exception as err:  # noqa: BLE001 - serve memory
            log.warning("read jurisdictions failed (serving memory): %s", err)
            return
        for r in rows or []:
            r = dict(r)
            self.jurisdictions[r["city"]] = {
                "city": r["city"], "name": r["name"], "state": r.get("state"), "authority": r.get("authority"),
                "rules": _loads(r["rules"]), "version": int(r.get("version") or 1),
                "updated_at": _iso(r.get("updated_at")), "updated_by": r.get("updated_by"),
            }

    async def list(self) -> List[Dict[str, Any]]:
        await self._pull()
        return [copy.deepcopy(self.jurisdictions[c]) for c in sorted(self.jurisdictions)]

    async def get(self, city: str) -> Optional[Dict[str, Any]]:
        await self._pull()
        j = self.jurisdictions.get(city)
        return copy.deepcopy(j) if j else None

    async def update_rules(self, city: str, rules: Dict[str, Any], by: str) -> Dict[str, Any]:
        cur = await self.get(city)
        if cur is None:
            raise KeyError(city)
        now = self.clock()
        new = {**cur, "rules": copy.deepcopy(rules), "version": cur["version"] + 1, "updated_at": _iso(now),
               "updated_by": by}
        self.jurisdictions[city] = new
        if self.pool is not None:
            try:
                await self._ensure_schema()
                await self.pool.execute(
                    _UPSERT.format(action="DO UPDATE SET rules = EXCLUDED.rules, version = EXCLUDED.version, "
                                          "updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by"),
                    city, new["name"], new["state"], new["authority"], json.dumps(new["rules"]), new["version"],
                    now, by)
            except Exception as err:  # noqa: BLE001
                log.warning("write jurisdiction %s failed (kept in memory): %s", city, err)
        return copy.deepcopy(new)

    async def add_audit(self, entry: Dict[str, Any]) -> Dict[str, Any]:
        now = self.clock()
        row = {"id": "lga_" + secrets.token_hex(6), **entry, "created_at": _iso(now)}
        self.audit_log.insert(0, row)
        del self.audit_log[AUDIT_KEEP:]
        if self.pool is not None:
            try:
                await self._ensure_schema()
                await self.pool.execute(
                    "INSERT INTO legal_audit (id, city, entry, created_at) VALUES ($1, $2, $3::jsonb, $4)",
                    row["id"], str(row.get("city") or ""), json.dumps(row, default=str), now)
            except Exception as err:  # noqa: BLE001
                log.warning("write legal_audit failed (kept in memory): %s", err)
        return copy.deepcopy(row)

    async def audit(self, limit: int = 50, city: Optional[str] = None) -> List[Dict[str, Any]]:
        limit = max(1, min(int(limit), AUDIT_KEEP))
        if self.pool is not None:
            try:
                await self._ensure_schema()
                if city:
                    rows = await self.pool.fetch(
                        "SELECT entry FROM legal_audit WHERE city = $1 ORDER BY created_at DESC LIMIT $2", city, limit)
                else:
                    rows = await self.pool.fetch(
                        "SELECT entry FROM legal_audit ORDER BY created_at DESC LIMIT $1", limit)
                return [_loads(dict(r)["entry"]) for r in rows or []]
            except Exception as err:  # noqa: BLE001
                log.warning("read legal_audit failed (serving memory): %s", err)
        return [copy.deepcopy(a) for a in self.audit_log if not city or a.get("city") == city][:limit]
