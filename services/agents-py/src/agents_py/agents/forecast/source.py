"""Where the Demand Forecast's inputs come from.

* Ride requests: Postgres ``rides`` (written by dispatch-go) - the last 2 hours
  plus +-30 min around the same time on each of the previous 7 days. Without
  Postgres (or if it fails) the last 15 minutes of requests come from
  dispatch-go's supply snapshot instead, with no history.
* Idle supply per cell: dispatch-go ``GET /v1/supply/snapshot``.
* Active scenarios: dispatch-go ``GET /v1/sim/config``.

Every input is optional: a missing one yields an empty list, never an error.
"""
from __future__ import annotations

import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable, Dict, List, Optional

from src.agents_py.agents.booking.gateway import DispatchGateway, GatewayError
from src.agents_py.agents.forecast import model as fm
from src.agents_py.core import infra

log = logging.getLogger("hexago.forecast")

SNAPSHOT_WINDOW_S = 900  # dispatch-go keeps 15 minutes of requests in memory

SQL_RECENT = """
    SELECT pickup_lat, pickup_lng, created_at FROM rides
    WHERE created_at >= $1 AND created_at <= $2
    ORDER BY created_at DESC LIMIT 50000
"""

SQL_HISTORY = """
    SELECT r.pickup_lat, r.pickup_lng, r.created_at FROM rides r
    JOIN generate_series(1, $2::int) AS d(n)
      ON r.created_at >= $1::timestamptz - make_interval(days => d.n) - interval '30 minutes'
     AND r.created_at <  $1::timestamptz - make_interval(days => d.n) + interval '30 minutes'
    LIMIT 50000
"""


@dataclass
class Loaded:
    recent: List[fm.RideRequest] = field(default_factory=list)
    history: List[fm.RideRequest] = field(default_factory=list)
    scenarios: List[fm.Scenario] = field(default_factory=list)
    supply: Dict[str, int] = field(default_factory=dict)
    source: str = "none"


def _from_rows(rows: List[Any]) -> List[fm.RideRequest]:
    return [fm.RideRequest(float(r["pickup_lat"]), float(r["pickup_lng"]), r["created_at"]) for r in rows]


def _from_snapshot(snap: Optional[Dict[str, Any]]) -> List[fm.RideRequest]:
    out = []
    for r in (snap or {}).get("requests") or []:
        at = fm._parse_ts(r.get("at"))
        if at is None or fm.cell_of(r.get("lat"), r.get("lng")) is None:
            continue
        out.append(fm.RideRequest(float(r["lat"]), float(r["lng"]), at))
    return out


class ForecastSource:
    def __init__(self, gateway: Optional[Any] = None,
                 pool: Callable[[], Awaitable[Any]] = infra.get_pool):
        self.gateway = gateway or DispatchGateway(timeout_s=5.0)
        self._pool = pool

    async def _postgres(self, now: dt.datetime) -> Optional[Loaded]:
        pool = await self._pool()
        if pool is None:
            return None
        try:
            recent = await pool.fetch(SQL_RECENT, now - dt.timedelta(seconds=fm.WINDOW_S), now)
            history = await pool.fetch(SQL_HISTORY, now, fm.HISTORY_DAYS)
        except Exception as err:  # noqa: BLE001 - fall back to the snapshot
            log.warning("forecast: postgres read failed: %s", err)
            return None
        return Loaded(recent=_from_rows(recent), history=_from_rows(history), source="postgres")

    async def load(self, now: dt.datetime) -> Loaded:
        snap: Optional[Dict[str, Any]] = None
        try:
            snap = await self.gateway.snapshot(SNAPSHOT_WINDOW_S)
        except GatewayError as err:
            log.warning("forecast: supply snapshot unavailable: %s", err)
        try:
            cfg = await self.gateway.sim_config()
        except GatewayError:
            cfg = None
        out = await self._postgres(now)
        if out is None:
            recent = _from_snapshot(snap)
            out = Loaded(recent=recent, source="dispatch-go" if snap is not None else "none")
        out.supply = fm.supply_by_cell(snap)
        out.scenarios = fm.parse_scenarios(cfg)
        return out
