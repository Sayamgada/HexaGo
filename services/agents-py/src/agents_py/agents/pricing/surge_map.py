"""Surge Equilibrium: a city-wide surge map on H3 resolution-8 cells.

Every ``SURGE_MAP_INTERVAL_S`` (30 s) the agent reads dispatch-go's supply
snapshot (every vehicle's position and status plus the ride requests of the
last 5 minutes), bins it into H3 res-8 cells and prices each cell with the same
pure function the fare quote uses (`quote.ratio_surge`):

    supply = idle vehicles, demand = ride requests,
    both counted over the cell and its ring-1 neighbours (~1.5 km across),
    so one stray request does not light up a cell.

Each cell's supply is scaled by its own normal pressure (requests per idle
car, an EWMA with a 10-minute half-life, seeded from the first observation
after a start): hotspots that are busy as usual stay at 1.0x and only demand
above the norm - a rain storm, a concert exit - raises prices. Cells need at
least ``MIN_DEMAND`` requests in their disk to surge. When a cell
crosses ``ALERT_AT`` (1.5x) the agent writes an agent event with a
deterministic template explanation; an LLM explanation is requested
asynchronously through `core.llm` (guardrails, budget, cache) and replaces the
template on the cell once it lands. The multiplier itself is never decided by
the LLM.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import logging
import time
from typing import Any, Callable, Dict, List, Optional, Set, Tuple

import h3
from pydantic import BaseModel, Field

from src.agents_py.agents.booking.gateway import DispatchGateway, GatewayError
from src.agents_py.agents.pricing.quote import SURGE_CAP, CellLoad, ratio_surge
from src.agents_py.core.events import EventStore, get_event_store
from src.agents_py.core.graph import agent
from src.agents_py.core.llm import LLMClient, get_llm

log = logging.getLogger("hexago.surge")

AGENT = "surge_equilibrium"
RESOLUTION = 8
WINDOW_S = 300           # ride requests of the last 5 minutes
MIN_DEMAND = 5           # requests in a cell's disk before it may surge
PRIOR = 3                # pseudo-observations of normal demand per idle car (damps small counts)
TOLERANCE = 1.2          # demand may run 20% above a cell's norm before it surges
ALERT_AT = 1.5           # log an event when a cell crosses this multiplier
BASELINE_HALF_LIFE_S = 600  # a cell's normal pressure adapts with a ~10 min half-life
BASELINE_FORGET_S = 1800    # cells unseen this long lose their baseline
REARM_BELOW = 1.3        # ...and alert again only after it fell below this
BUSY = ("en_route", "on_trip")


class SurgeExplanation(BaseModel):
    explanation: str = Field(..., max_length=500)


def _disk_counts(points: Dict[str, int], cell: str) -> int:
    return sum(points.get(c, 0) for c in h3.grid_disk(cell, 1))


def bin_snapshot(snapshot: Dict[str, Any], resolution: int = RESOLUTION) -> Dict[str, Dict[str, int]]:
    """Per cell with recent requests: demand, idle supply and busy cars over the cell's ring-1 disk."""
    idle: Dict[str, int] = {}
    busy: Dict[str, int] = {}
    demand: Dict[str, int] = {}

    def cell_of(p: Dict[str, Any]) -> Optional[str]:
        try:
            return h3.latlng_to_cell(float(p["lat"]), float(p["lng"]), resolution)
        except (KeyError, TypeError, ValueError, h3.H3BaseException):
            return None

    for d in snapshot.get("drivers") or []:
        cell = cell_of(d)
        status = str(d.get("status", ""))
        if cell is None:
            continue
        if status == "idle":
            idle[cell] = idle.get(cell, 0) + 1
        elif status in BUSY:
            busy[cell] = busy.get(cell, 0) + 1
    for r in snapshot.get("requests") or []:
        cell = cell_of(r)
        if cell is not None:
            demand[cell] = demand.get(cell, 0) + 1
    return {c: {"demand": _disk_counts(demand, c), "supply": _disk_counts(idle, c), "busy": _disk_counts(busy, c)}
            for c in demand}


def pressure(demand: int, supply: int) -> float:
    """Requests per idle car (an empty disk counts as one car)."""
    return demand / max(supply, 1)


def price_cells(stats: Dict[str, Dict[str, int]], baseline: Optional[Dict[str, float]] = None,
                default_baseline: float = 1.0, min_demand: int = MIN_DEMAND) -> List[Dict[str, Any]]:
    """Price each cell with `quote.ratio_surge`.

    The curve sees demand against idle supply scaled by the cell's usual
    pressure (requests per idle car, never below 1) and a 20% tolerance, with
    PRIOR pseudo-observations of normal load added to both sides so a handful
    of requests cannot spike a cell. A hotspot running at its normal level
    stays at 1.0x; demand above the norm - a rain storm, a concert letting out
    - raises the price. Multipliers are rounded to 0.1x. Pure: same inputs,
    same cells.
    """
    baseline = baseline or {}
    cells = []
    for cell, st in stats.items():
        d, s = st["demand"], st["supply"]
        norm = max(1.0, baseline.get(cell, default_baseline))
        load = CellLoad(cell=cell, supply=(s + PRIOR) * norm * TOLERANCE, demand=d + PRIOR * norm)
        mult = round(ratio_surge(load), 1) if d >= min_demand else 1.0
        lat, lng = h3.cell_to_latlng(cell)
        cells.append({
            "cell": cell,
            "multiplier": mult,
            "label": f"{mult:.1f}x",
            "supply": s,
            "demand": d,
            "busy": st["busy"],
            "pressure": round(pressure(d, s), 3),
            "baseline": round(norm, 3),
            "center": {"lat": round(lat, 6), "lng": round(lng, 6)},
            "boundary": [[round(a, 6), round(b, 6)] for a, b in h3.cell_to_boundary(cell)],
        })
    cells.sort(key=lambda c: (-c["multiplier"], -c["demand"], c["cell"]))
    return cells


def compute_cells(snapshot: Dict[str, Any], baseline: Optional[Dict[str, float]] = None,
                  default_baseline: float = 1.0, resolution: int = RESOLUTION,
                  min_demand: int = MIN_DEMAND) -> List[Dict[str, Any]]:
    """bin_snapshot + price_cells: the surge map for one snapshot, highest multiplier first."""
    return price_cells(bin_snapshot(snapshot, resolution), baseline, default_baseline, min_demand)


def template_explanation(c: Dict[str, Any]) -> str:
    ratio_txt = (f"{c['demand'] / c['supply']:.1f} requests per idle car" if c["supply"] else "no idle car nearby")
    return (f"{c['demand']} ride requests in the last {WINDOW_S // 60} min against {c['supply']} idle cars around "
            f"cell {c['cell']} ({ratio_txt}, usually {c.get('baseline', 1.0):.1f}); the supply/demand curve prices "
            f"it at {c['multiplier']:.1f}x (cap {SURGE_CAP}x) to pull nearby drivers in.")


class SurgeMap:
    def __init__(self, gateway: Optional[Any] = None, store: Optional[EventStore] = None,
                 llm: Optional[LLMClient] = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)):
        self.gateway = gateway or DispatchGateway(timeout_s=5.0)
        self._store, self._llm = store, llm
        self.clock = clock
        self.cells: List[Dict[str, Any]] = []
        self.as_of: Optional[dt.datetime] = None
        self.source = "none"
        self.runs = 0
        self.alerted: Set[str] = set()
        self.explanations: Dict[str, Tuple[str, str]] = {}  # cell -> (text, source)
        self.baseline: Dict[str, float] = {}       # cell -> normal pressure
        self._seen: Dict[str, dt.datetime] = {}
        self._surging = False
        self._lock = asyncio.Lock()
        self._tasks: Set[asyncio.Task] = set()

    async def store(self) -> EventStore:
        if self._store is None:
            self._store = await get_event_store()
        return self._store

    async def llm(self) -> LLMClient:
        if self._llm is None:
            self._llm = await get_llm()
        return self._llm

    async def idle(self) -> None:
        while self._tasks:
            await asyncio.gather(*list(self._tasks), return_exceptions=True)

    def stale(self, max_age_s: float) -> bool:
        return self.as_of is None or (self.clock() - self.as_of).total_seconds() > max_age_s

    async def refresh(self) -> Dict[str, Any]:
        """Fetch the snapshot, recompute the cells, alert on 1.5x crossings."""
        async with self._lock:
            start = time.perf_counter()
            now = self.clock()
            try:
                snap = await self.gateway.snapshot(WINDOW_S)
                self.source = "dispatch-go"
            except GatewayError as err:
                log.warning("surge map: dispatch-go snapshot unavailable: %s", err)
                self.source = "unavailable"
                self.as_of = now
                return self.view()
            stats = bin_snapshot(snap)
            cells = price_cells(stats, self._baselines_for(stats), self._default_baseline())
            self._learn(stats, now)
            self.cells, self.as_of = cells, now
            self.runs += 1
            await self._alerts(cells, (time.perf_counter() - start) * 1000)
            return self.view()

    def _default_baseline(self) -> float:
        """For a cell never seen before: the median normal pressure of known cells (1.0 when none)."""
        if not self.baseline:
            return 1.0
        vals = sorted(self.baseline.values())
        return vals[len(vals) // 2]

    def _baselines_for(self, stats: Dict[str, Dict[str, int]]) -> Dict[str, float]:
        if self.runs == 0 and not self.baseline:  # first look after a start: today's level is the norm
            return {c: pressure(st["demand"], st["supply"]) for c, st in stats.items()}
        return self.baseline

    def _learn(self, stats: Dict[str, Dict[str, int]], now: dt.datetime) -> None:
        last = max(self._seen.values(), default=None)
        dt_s = (now - last).total_seconds() if last else 30.0
        alpha = 1 - 0.5 ** (max(dt_s, 1.0) / BASELINE_HALF_LIFE_S)
        first = self.runs == 0 and not self.baseline
        default = self._default_baseline()
        for c, st in stats.items():
            p = pressure(st["demand"], st["supply"])
            if c not in self.baseline:
                self.baseline[c] = p if first else default
            self.baseline[c] += alpha * (p - self.baseline[c])
            self._seen[c] = now
        for c, seen in list(self._seen.items()):
            if (now - seen).total_seconds() > BASELINE_FORGET_S:
                self._seen.pop(c, None)
                self.baseline.pop(c, None)

    async def _alerts(self, cells: List[Dict[str, Any]], latency_ms: float) -> None:
        by_id = {c["cell"]: c for c in cells}
        for cell in list(self.alerted):
            if by_id.get(cell, {}).get("multiplier", 1.0) < REARM_BELOW:
                self.alerted.discard(cell)
                self.explanations.pop(cell, None)
        crossed = [c for c in cells if c["multiplier"] >= ALERT_AT and c["cell"] not in self.alerted]
        store = await self.store()
        for i, c in enumerate(crossed):
            self.alerted.add(c["cell"])
            text = template_explanation(c)
            self.explanations[c["cell"]] = (text, "template")
            await store.record_event(
                AGENT, f"surge map cell {c['cell']} (demand {c['demand']}, idle {c['supply']})",
                f"surge {c['label']} in cell {c['cell'][:9]}... near {c['center']['lat']:.3f},{c['center']['lng']:.3f}",
                text, 1.0, latency_ms, False, None,
                {"kind": "surge_alert", "cell": c["cell"], "multiplier": c["multiplier"], "supply": c["supply"],
                 "demand": c["demand"], "model": "supply_demand_ratio"},
            )
            if i == 0:  # at most one LLM explanation per run
                self._spawn(self._explain(c))
        surging = any(c["multiplier"] > 1.0 for c in cells)
        if self._surging and not surging:
            await store.record_event(AGENT, "surge map", "surge cleared: every cell back to 1.0x",
                                     "Idle supply caught up with the last 5 minutes of requests everywhere.",
                                     1.0, latency_ms, False, None, {"kind": "surge_cleared"})
        self._surging = surging

    def _spawn(self, coro) -> None:
        task = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def _explain(self, c: Dict[str, Any]) -> None:
        start = time.perf_counter()
        prompt = (
            "You are HexaGo's Surge Equilibrium agent in Bengaluru. A deterministic supply/demand curve priced the "
            "H3 cell below. In two short sentences for riders and drivers, explain why the price is up and what "
            "will bring it down. Use only these facts.\n"
            f"cell: {c['cell']} near {c['center']}\nmultiplier: {c['multiplier']}x\n"
            f"requests (last {WINDOW_S // 60} min, cell + neighbours): {c['demand']}\nidle cars: {c['supply']}\n"
            f"cars on trips nearby: {c['busy']}"
        )
        try:
            res = await (await self.llm()).complete_json(
                prompt, SurgeExplanation, AGENT, lambda: {"explanation": template_explanation(c)})
        except Exception as err:  # noqa: BLE001 - an explanation must never break the agent
            log.warning("surge explanation failed: %s", err)
            return
        if c["cell"] in self.alerted:
            self.explanations[c["cell"]] = (res.data["explanation"], "template" if res.used_fallback else "llm")
        if res.used_fallback:
            return  # the template event is already in the feed
        await (await self.store()).record_event(
            AGENT, f"explain surge in cell {c['cell']}", f"why {c['label']} near {c['center']['lat']:.3f},"
            f"{c['center']['lng']:.3f}", res.data["explanation"], 0.9, (time.perf_counter() - start) * 1000,
            False, None, {"kind": "surge_reasoning", "cell": c["cell"], "tokens": res.tokens, "model": res.model},
        )

    def view(self) -> Dict[str, Any]:
        cells = []
        for c in self.cells:
            out = dict(c)
            if c["cell"] in self.explanations:
                out["explanation"], out["explanation_source"] = self.explanations[c["cell"]]
            cells.append(out)
        return {
            "as_of": self.as_of.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ") if self.as_of else None,
            "resolution": RESOLUTION,
            "window_s": WINDOW_S,
            "model": "supply_demand_ratio",
            "source": self.source,
            "max_multiplier": max((c["multiplier"] for c in self.cells), default=1.0),
            "surging": sum(1 for c in self.cells if c["multiplier"] > 1.0),
            "cells": cells,
        }


_map: Optional[SurgeMap] = None


def get_map() -> SurgeMap:
    global _map
    if _map is None:
        _map = SurgeMap()
    return _map


def set_map(m: Optional[SurgeMap]) -> None:
    global _map
    _map = m


@agent(AGENT, "Prices H3 res-8 cells from live supply and demand every 30 s; explains 1.5x crossings.")
async def recompute(m: Optional[SurgeMap] = None) -> Dict[str, Any]:
    return await (m or get_map()).refresh()


async def surge_loop(interval_s: float, m: Optional[SurgeMap] = None,
                     sleep: Callable[[float], Any] = asyncio.sleep) -> None:
    """Recompute now, then every interval_s seconds, until cancelled."""
    while True:
        try:
            await recompute(m)
        except asyncio.CancelledError:
            raise
        except Exception as err:  # noqa: BLE001 - the loop must survive any single failure
            log.warning("surge map recompute failed: %s", err)
        await sleep(interval_s)

