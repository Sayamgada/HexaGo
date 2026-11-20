"""Demand Forecast agent: hex-level demand now / +15 / +30 min, recomputed every 60 s.

Each run loads ride requests (Postgres, see ``source.py``), runs the pure model
in ``model.py`` and logs one summary event (``kind=forecast_run``). The top-5
hotspots at +15 min get a one-line natural-language explanation from the LLM,
asynchronously and through ``core.llm`` (guardrails, budget, cache); until it
lands - or when the LLM is unavailable - each cell carries its deterministic
template. LLM calls are event-driven: only when a top-5 cell lacks a fresh
explanation and either ``LLM_GAP_S`` has passed or the active scenarios
changed. Whenever the set of active scenarios changes, every LLM line is
dropped at once (and an answer still in flight for the old set is discarded),
so no "heavy rain" text outlives a storm. The forecast numbers themselves
never come from the LLM.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import logging
import time
from typing import Any, Callable, Dict, List, Optional, Set, Tuple

from pydantic import BaseModel, Field

from src.agents_py.agents.forecast import model as fm
from src.agents_py.agents.forecast.source import ForecastSource
from src.agents_py.config import settings
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore, get_event_store
from src.agents_py.core.graph import agent
from src.agents_py.core.llm import LLMClient, get_llm

log = logging.getLogger("hexago.forecast")

AGENT = "demand_forecast"
DESCRIPTION = ("Forecasts ride demand per H3 res-8 cell now / +15 / +30 min every 60 s "
               "(Holt smoothing + time-of-day + scenario boost); explains the top-5 hotspots.")
EXPLAIN_TOP = 5
EXPLAIN_HORIZON = 15
LLM_GAP_S = 900            # at most one LLM call per 15 min unless the scenarios change
EXPLANATION_TTL_S = 1800   # an LLM line is shown for 30 min


class HotspotLine(BaseModel):
    h3: str
    explanation: str = Field(..., max_length=240)


class HotspotExplanations(BaseModel):
    items: List[HotspotLine] = Field(default_factory=list, max_length=10)


def _utcnow() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


class ForecastAgent:
    def __init__(self, source: Optional[Any] = None, store: Optional[EventStore] = None,
                 llm: Optional[LLMClient] = None, clock: Callable[[], dt.datetime] = _utcnow,
                 center: Optional[Tuple[float, float]] = None):
        self.source = source or ForecastSource()
        self._store, self._llm = store, llm
        self.clock = clock
        self.center = center or (settings.CITY_CENTER_LAT, settings.CITY_CENTER_LNG)
        self.cells: Dict[int, List[Dict[str, Any]]] = {h: [] for h in fm.HORIZONS}
        self.scenarios: List[fm.Scenario] = []
        self.source_name = "none"
        self.as_of: Optional[dt.datetime] = None
        self.runs = 0
        self.explanations: Dict[str, Tuple[str, dt.datetime]] = {}  # h3 -> (LLM text, when)
        self._llm_calls = 0
        self._last_llm: Optional[dt.datetime] = None
        self._last_llm_scenarios: Optional[Tuple[str, ...]] = None
        self._scenario_key: Tuple[str, ...] = ()
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

    def active_scenarios(self, now: Optional[dt.datetime] = None) -> Tuple[str, ...]:
        """Names of the scenarios still running at ``now`` (an ended one only fades in the numbers)."""
        now = now or self.clock()
        return tuple(sorted({s.name for s in self.scenarios if s.until > now}))

    def stale(self, max_age_s: float) -> bool:
        return self.as_of is None or (self.clock() - self.as_of).total_seconds() > max_age_s

    async def refresh(self) -> Dict[str, Any]:
        """Load inputs, forecast every horizon, log the run, maybe ask the LLM about the hotspots."""
        async with self._lock:
            start = time.perf_counter()
            now = self.clock()
            loaded = await self.source.load(now)
            self.cells = fm.forecast(loaded.recent, loaded.history, now, loaded.scenarios, loaded.supply, self.center)
            self.scenarios, self.source_name, self.as_of = loaded.scenarios, loaded.source, now
            self.runs += 1
            key = self.active_scenarios(now)
            if key != self._scenario_key:
                self.explanations.clear()  # regenerate for the new scenarios
                self._scenario_key = key
            top = fm.hotspots(self.cells[EXPLAIN_HORIZON], EXPLAIN_TOP)
            await self._log_run(loaded, top, (time.perf_counter() - start) * 1000)
            self._maybe_explain(top, now)
            return {"cells": len(self.cells[0]), "source": self.source_name,
                    "hotspots": [c["h3"] for c in top], "scenarios": [s.name for s in self.scenarios]}

    async def _log_run(self, loaded: Any, top: List[Dict[str, Any]], latency_ms: float) -> None:
        names = [s.name for s in self.scenarios]
        scen = f"; scenarios: {', '.join(names)}" if names else ""
        if top:
            t = top[0]
            decision = (f"forecast {len(self.cells[0])} cells; top +{EXPLAIN_HORIZON} min hotspot {t['h3'][:9]}... "
                        f"~{t['expected_requests_per_hr']:.0f} rides/h, ~₹{t['expected_earnings_per_hr']}/h{scen}")
        else:
            decision = f"forecast: no ride requests in the last 2 h{scen}"
        reasoning = (f"Holt smoothing over 5-min buckets of {len(loaded.recent)} requests (last 2 h, from "
                     f"{self.source_name}), {len(loaded.history)} same-hour requests on previous days, "
                     f"time-of-day seasonality and scenario boosts.")
        await (await self.store()).record_event(
            AGENT, f"{len(loaded.recent)} recent + {len(loaded.history)} historical ride requests", decision,
            reasoning, 1.0, latency_ms, False, None,
            {"kind": "forecast_run", "cells": len(self.cells[0]), "hotspots": [c["h3"] for c in top],
             "scenarios": names, "source": self.source_name, "model": "holt_seasonal"},
        )

    def _fresh(self, h3_id: str, now: dt.datetime) -> bool:
        got = self.explanations.get(h3_id)
        return got is not None and (now - got[1]).total_seconds() <= EXPLANATION_TTL_S

    def _maybe_explain(self, top: List[Dict[str, Any]], now: dt.datetime) -> None:
        if not top or all(self._fresh(c["h3"], now) for c in top):
            return
        names = self._scenario_key
        gap_ok = self._last_llm is None or (now - self._last_llm).total_seconds() >= LLM_GAP_S
        if not gap_ok and names == self._last_llm_scenarios:
            return
        self._last_llm, self._last_llm_scenarios = now, names
        self._llm_calls += 1
        task = asyncio.create_task(self._explain([dict(c) for c in top], now, names))
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def _explain(self, top: List[Dict[str, Any]], now: dt.datetime, key: Tuple[str, ...] = ()) -> None:
        start = time.perf_counter()
        scen = ", ".join(key) or "none"
        facts = "\n".join(
            f"- {c['h3']}: ~{c['expected_requests_per_hr']:.0f} rides/h in {EXPLAIN_HORIZON} min, "
            f"{c['recent_requests']} requests in the last 30 min ({c['trend']}), {c['supply']} idle cars, "
            f"~INR {c['expected_earnings_per_hr']}/h, near {c['center']['lat']:.4f},{c['center']['lng']:.4f}"
            for c in top)
        prompt = (
            "You are HexaGo's Demand Forecast agent in Bengaluru. A deterministic model forecast the H3 cells "
            "below. For each cell write ONE short sentence (max 25 words) telling a driver why demand is "
            "expected there. Use only these facts and well-known Bengaluru landmarks near the coordinates.\n"
            f"active scenarios: {scen}\n{facts}"
        )
        try:
            res = await (await self.llm()).complete_json(prompt, HotspotExplanations, AGENT, lambda: {"items": []})
        except Exception as err:  # noqa: BLE001 - an explanation must never break the agent
            log.warning("forecast explanation failed: %s", err)
            return
        if res.used_fallback:
            return  # the template stays
        if key != self._scenario_key:
            return  # the scenarios changed while the LLM was answering: drop the stale lines
        wanted = {c["h3"] for c in top}
        got = {i["h3"]: i["explanation"] for i in res.data.get("items", []) if i.get("h3") in wanted}
        for h3_id, text in got.items():
            self.explanations[h3_id] = (text, now)
        if got:
            await (await self.store()).record_event(
                AGENT, f"explain {len(got)} demand hotspots", f"explained {len(got)} of the top-{EXPLAIN_TOP} hotspots",
                " | ".join(got.values()), 0.9, (time.perf_counter() - start) * 1000, False, None,
                {"kind": "forecast_reasoning", "cells": sorted(got), "tokens": res.tokens, "model": res.model},
            )

    def view(self, horizon: int = 0) -> Dict[str, Any]:
        if horizon not in fm.HORIZONS:
            raise ValueError(f"horizon must be one of {fm.HORIZONS}")
        now = self.clock()
        cells = []
        for c in self.cells.get(horizon, []):
            out = dict(c)
            if self._fresh(c["h3"], now):
                out["explanation"], out["explanation_source"] = self.explanations[c["h3"]][0], "llm"
            cells.append(out)
        return {
            "as_of": self.as_of.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ") if self.as_of else None,
            "horizon": horizon,
            "resolution": fm.RESOLUTION,
            "model": "holt_seasonal",
            "source": self.source_name,
            "scenarios": [s.name for s in self.scenarios],
            "runs": self.runs,
            "hotspots": [c["h3"] for c in fm.hotspots(cells, EXPLAIN_TOP)],
            "cells": cells,
        }


_agent: Optional[ForecastAgent] = None


def get_agent() -> ForecastAgent:
    global _agent
    if _agent is None:
        _agent = ForecastAgent()
    return _agent


def set_agent(a: Optional[ForecastAgent]) -> None:
    global _agent
    _agent = a


@agent(AGENT, DESCRIPTION)
async def run_forecast(a: Optional[ForecastAgent] = None) -> Dict[str, Any]:
    return await (a or get_agent()).refresh()


def apply_flag() -> None:
    """FORECAST_ENABLED=false takes the agent off the live roster (status "planned")."""
    if settings.FORECAST_ENABLED:
        registry.register(AGENT, DESCRIPTION, run_forecast)
    else:
        registry.unregister(AGENT)


async def forecast_loop(interval_s: float, a: Optional[Any] = None,
                        sleep: Callable[[float], Any] = asyncio.sleep) -> None:
    """Forecast now, then every interval_s seconds, until cancelled."""
    while True:
        try:
            await (a or get_agent()).refresh()
        except asyncio.CancelledError:
            raise
        except Exception as err:  # noqa: BLE001 - the loop must survive any single failure
            log.warning("demand forecast failed: %s", err)
        await sleep(interval_s)
