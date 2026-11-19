"""Ops Supervisor "Autonomous Ops Report": every 5 minutes and on demand.

One run:

1. Collect: the agent roster with last-hour decisions and fallback rates (from
   ``agent_events``), dispatch-go's ``/v1/metrics/dispatch`` and
   ``/v1/ledger/summary``, safety incidents and the LLM budget. A source that
   fails is reported as unavailable; the report is still written.
2. Detect anomalies with the pure rules in ``report.py`` and take the
   auto-actions below (each new anomaly is also an ``ops_anomaly`` agent event):

   * silent agent       -> re-run its zero-argument handler (else alert)
   * ledger imbalance   -> trigger a Settlement ledger audit
   * high fallback rate -> alert; the agent keeps serving through its deterministic fallback
   * stale incident     -> flag for Ops Supervisor review (closing still needs a confirmed resolution)

3. Build the deterministic markdown report (template executive summary), keep
   it in history (memory + ``ops_reports`` in Postgres) and write an
   ``ops_report`` agent event. The LLM then writes the executive summary
   paragraph asynchronously through ``core.llm``; when it lands the report is
   re-rendered with it, otherwise the template stays.
"""
from __future__ import annotations

import asyncio
import collections
import datetime as dt
import inspect
import json
import logging
import secrets
import time
from typing import Any, Awaitable, Callable, Deque, Dict, List, Optional, Set, Tuple

import httpx
from pydantic import BaseModel, Field

from src.agents_py.agents.supervisor.report import (
    ReportInputs, build_report, detect_anomalies, pct, render_markdown, rupees,
)
from src.agents_py.config import settings
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore, get_event_store
from src.agents_py.core.llm import LLMClient, get_llm

log = logging.getLogger("hexago.ops_report")

AGENT = "ops_supervisor"
HISTORY = 100
HANDLER_TIMEOUT_S = 15.0

DDL = """
CREATE TABLE IF NOT EXISTS ops_reports (
    id            TEXT PRIMARY KEY,
    generated_at  TIMESTAMPTZ NOT NULL,
    report        JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS ops_reports_generated_idx ON ops_reports (generated_at DESC);
"""


class ExecSummary(BaseModel):
    summary: str = Field(..., min_length=20, max_length=900)


class DispatchSources:
    """dispatch-go reads for the report: dispatch metrics and the ledger summary."""

    def __init__(self, base_url: Optional[str] = None, transport: Optional[httpx.AsyncBaseTransport] = None,
                 timeout_s: float = 8.0):
        self.base_url = (base_url or settings.DISPATCH_URL).rstrip("/")
        self.transport, self.timeout_s = transport, timeout_s

    async def _get(self, path: str) -> Dict[str, Any]:
        async with httpx.AsyncClient(base_url=self.base_url, timeout=self.timeout_s, transport=self.transport) as c:
            resp = await c.get(path)
        if resp.status_code >= 400:
            raise RuntimeError(f"GET {path}: HTTP {resp.status_code}")
        return resp.json()

    async def metrics(self) -> Dict[str, Any]:
        return await self._get("/v1/metrics/dispatch")

    async def ledger(self) -> Dict[str, Any]:
        return await self._get("/v1/ledger/summary")


async def _sentinel_incidents() -> List[Dict[str, Any]]:
    from src.agents_py.agents.safety.sentinel import get_sentinel

    sentinel = await get_sentinel()
    return [sentinel.summary(i) for i in await sentinel.store.list_incidents(200)]


def _loads(v: Any) -> Dict[str, Any]:
    if isinstance(v, (str, bytes)):
        try:
            return json.loads(v)
        except ValueError:
            return {}
    return dict(v or {})


def _row(rep: Dict[str, Any]) -> Dict[str, Any]:
    return {"id": rep["id"], "generated_at": rep["generated_at"], "trigger": rep["trigger"],
            "anomalies": len(rep.get("anomalies") or []), "summary_source": rep.get("summary_source"),
            "human_interventions": rep.get("human_interventions", 0), "headline": str(rep.get("summary", ""))[:200]}


def _runnable(fn: Callable[..., Any]) -> bool:
    try:
        params = inspect.signature(fn).parameters.values()
    except (TypeError, ValueError):
        return False
    return all(p.default is not p.empty or p.kind in (p.VAR_POSITIONAL, p.VAR_KEYWORD) for p in params)


class OpsReporter:
    def __init__(self, sources: Optional[DispatchSources] = None, store: Optional[EventStore] = None,
                 llm: Optional[LLMClient] = None, pool: Any = None,
                 incidents: Optional[Callable[[], Awaitable[List[Dict[str, Any]]]]] = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)):
        self.sources = sources or DispatchSources()
        self._store, self._llm = store, llm
        self.pool = pool
        self.incidents = incidents or _sentinel_incidents
        self.clock = clock
        self.reports: Deque[Dict[str, Any]] = collections.deque(maxlen=HISTORY)
        self._alerted: Set[Tuple[str, str]] = set()
        self._lock = asyncio.Lock()
        self._tasks: Set[asyncio.Task] = set()
        self._schema_ready = False

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

    # ------------------------------------------------------------ collect

    async def collect(self) -> ReportInputs:
        now = self.clock()
        inp = ReportInputs(now=now)

        async def grab(name: str, coro: Awaitable[Any]) -> Any:
            try:
                got = await coro
                inp.sources[name] = "ok"
                return got
            except Exception as err:  # noqa: BLE001 - a missing source is part of the report
                inp.sources[name] = f"unavailable: {type(err).__name__}: {err}"[:200]
                return None

        inp.agents = await grab("agents", registry.list_agents(await self.store(), now)) or []
        inp.metrics = await grab("metrics", self.sources.metrics())
        inp.ledger = await grab("ledger", self.sources.ledger())
        inp.incidents = await grab("incidents", self.incidents()) or []
        inp.budget = await grab("llm", (await self.llm()).budget())
        return inp

    # ------------------------------------------------------------ auto-actions

    async def _rerun(self, aid: str) -> Tuple[Optional[str], str]:
        reg = registry.registered().get(aid)
        if reg is None or not _runnable(reg.handler):
            return None, ""
        try:
            res = reg.handler()
            if inspect.isawaitable(res):
                await asyncio.wait_for(res, HANDLER_TIMEOUT_S)
            return reg.name, "ok"
        except Exception as err:  # noqa: BLE001
            return reg.name, f"failed: {type(err).__name__}: {err}"[:200]

    async def act(self, anomaly: Dict[str, Any]) -> Dict[str, Any]:
        kind, subject = anomaly["kind"], anomaly["subject"]
        out = {"kind": kind, "subject": subject}
        if kind == "silent_agent":
            name, result = await self._rerun(subject)
            if name:
                return {**out, "action": f"re-ran {name}", "result": result}
            where = registry.EXTERNAL.get(subject)
            reason = f"runs in {where}" if where and subject not in registry.registered() \
                else "no zero-argument handler to re-run"
            return {**out, "action": f"alerted the Ops feed ({reason})", "result": "ok"}
        if kind == "ledger_imbalance":
            name, result = await self._rerun("settlement")
            if name:
                return {**out, "action": "triggered a Settlement ledger audit", "result": result}
            return {**out, "action": "alerted the Ops feed (Settlement agent not running here)", "result": "ok"}
        if kind == "stale_incident":
            return {**out, "action": "flagged for Ops Supervisor review; the driver stays frozen until a confirmed "
                                     "resolution", "result": "pending_review"}
        return {**out, "action": "alerted the Ops feed; the agent keeps serving through its deterministic fallback",
                "result": "ok"}

    # ------------------------------------------------------------ run

    async def run(self, trigger: str = "manual") -> Dict[str, Any]:
        async with self._lock:
            start = time.perf_counter()
            inp = await self.collect()
            anomalies = detect_anomalies(inp)
            actions = [await self.act(a) for a in anomalies]
            report_id = f"opr_{inp.now.astimezone(dt.timezone.utc):%Y%m%dT%H%M%S}_{secrets.token_hex(2)}"
            rep = build_report(inp, anomalies, actions, trigger=trigger, report_id=report_id)
            self.reports.appendleft(rep)
            await self._persist(rep)
            store = await self.store()
            current = set()
            for a, act in zip(anomalies, actions):
                key = (a["kind"], str(a["subject"]))
                current.add(key)
                if key in self._alerted:
                    continue
                await store.record_event(
                    AGENT, f"ops report {report_id}", f"anomaly {a['kind']}: {a['subject']}",
                    f"{a['detail']}. Auto-action: {act['action']} ({act['result']}).", 1.0,
                    (time.perf_counter() - start) * 1000, False, None,
                    {"kind": "ops_anomaly", "anomaly": a["kind"], "subject": a["subject"], "report_id": report_id,
                     "action": act["action"], "result": act["result"]})
            self._alerted = current
            k = rep["kpis"]
            await store.record_event(
                AGENT, f"agent_events, metrics, incidents, ledger ({trigger})",
                f"ops report {report_id}: {len(anomalies)} anomal{'y' if len(anomalies) == 1 else 'ies'}, "
                "0 human interventions",
                rep["summary"], 1.0, (time.perf_counter() - start) * 1000, False, None,
                {"kind": "ops_report", "report_id": report_id, "anomalies": len(anomalies),
                 "decisions_last_hour": k["decisions_last_hour"], "fallback_rate": k["fallback_rate"],
                 "model": "deterministic_report"})
            self._spawn(self._summarize(rep))
            return rep

    def _spawn(self, coro) -> None:
        task = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def _summarize(self, rep: Dict[str, Any]) -> None:
        start = time.perf_counter()
        k, m = rep["kpis"], rep.get("money") or {}
        facts = {
            "decisions_last_hour": k["decisions_last_hour"], "fallback_rate": pct(k["fallback_rate"]),
            "decisions_today": k["decisions_today"], "matches_today": k["matches_today"],
            "rides_settled_today": k["rides_settled_today"], "rides_in_progress": k["rides_in_progress"],
            "dispatch_p99_ms": k["dispatch_p99_ms"], "active_agents": f"{k['active_agents']} of {k['total_agents']}",
            "trial_balance": rupees(m.get("trial_balance_paise")) if m else "unavailable",
            "platform_revenue": rupees(m.get("platform_revenue_paise")) if m else "unavailable",
            "incidents": {x: rep["incidents"][x] for x in ("total", "open", "escalated", "stale")},
            "anomalies": [f"{a['kind']} {a['subject']}: {a['detail']}" for a in rep["anomalies"]],
            "auto_actions": [f"{a['action']} ({a['result']})" for a in rep["actions"]],
            "human_interventions": 0,
        }
        prompt = (
            "You are HexaGo's Ops Supervisor agent writing the executive summary of the Autonomous Ops Report for "
            "a ride-hailing platform in Bengaluru run entirely by AI agents. Write one paragraph of 3-4 sentences "
            "for an executive: how the platform is doing, what went wrong (if anything) and what the agents did "
            "about it, and that no human intervened. Use only these facts and figures:\n"
            f"{json.dumps(facts, default=str)}"
        )
        try:
            res = await (await self.llm()).complete_json(prompt, ExecSummary, AGENT,
                                                         lambda: {"summary": rep["summary"]})
        except Exception as err:  # noqa: BLE001 - the summary must never break the report
            log.warning("ops report summary failed: %s", err)
            return
        if res.used_fallback:
            return
        rep["summary"], rep["summary_source"], rep["summary_model"] = res.data["summary"], "llm", res.model
        rep["markdown"] = render_markdown(rep)
        await self._persist(rep)
        await (await self.store()).record_event(
            AGENT, f"write executive summary for {rep['id']}", f"ops report {rep['id']}: executive summary written",
            res.data["summary"], 0.9, (time.perf_counter() - start) * 1000, False, None,
            {"kind": "ops_report_summary", "report_id": rep["id"], "tokens": res.tokens, "model": res.model})

    # ------------------------------------------------------------ persistence

    async def _ensure_schema(self) -> None:
        if self._schema_ready:
            return
        if await self.pool.fetchval("SELECT to_regclass('public.ops_reports')") is None:
            await self.pool.execute(DDL)
        self._schema_ready = True

    async def _persist(self, rep: Dict[str, Any]) -> None:
        if self.pool is None:
            return
        try:
            await self._ensure_schema()
            await self.pool.execute(
                "INSERT INTO ops_reports (id, generated_at, report) VALUES ($1, $2, $3::jsonb) "
                "ON CONFLICT (id) DO UPDATE SET report = EXCLUDED.report",
                rep["id"], self.clock(), json.dumps(rep, default=str))
        except Exception as err:  # noqa: BLE001
            log.warning("write ops_reports failed (kept in memory): %s", err)

    def latest(self) -> Optional[Dict[str, Any]]:
        return self.reports[0] if self.reports else None

    async def latest_or_stored(self) -> Optional[Dict[str, Any]]:
        if self.reports:
            return self.reports[0]
        stored = await self._stored(1)
        return stored[0] if stored else None

    async def _stored(self, limit: int) -> List[Dict[str, Any]]:
        if self.pool is None:
            return []
        try:
            await self._ensure_schema()
            rows = await self.pool.fetch("SELECT report FROM ops_reports ORDER BY generated_at DESC LIMIT $1", limit)
            return [_loads(dict(r)["report"]) for r in rows or []]
        except Exception as err:  # noqa: BLE001
            log.warning("read ops_reports failed (serving memory): %s", err)
            return []

    async def history(self, limit: int = 20) -> List[Dict[str, Any]]:
        limit = max(1, min(int(limit), HISTORY))
        stored = await self._stored(limit)
        reports = stored or list(self.reports)
        return [_row(r) for r in reports[:limit]]

    async def get(self, report_id: str) -> Optional[Dict[str, Any]]:
        for r in self.reports:
            if r["id"] == report_id:
                return r
        if self.pool is None:
            return None
        try:
            await self._ensure_schema()
            row = await self.pool.fetchrow("SELECT report FROM ops_reports WHERE id = $1", report_id)
        except Exception as err:  # noqa: BLE001
            log.warning("read ops_reports failed: %s", err)
            return None
        return _loads(dict(row)["report"]) if row else None


_reporter: Optional[OpsReporter] = None


async def get_reporter() -> OpsReporter:
    global _reporter
    from src.agents_py.core import infra

    if _reporter is None:
        _reporter = OpsReporter(pool=await infra.get_pool())
    elif _reporter.pool is None:
        _reporter.pool = await infra.get_pool()
    return _reporter


def set_reporter(r: Optional[OpsReporter]) -> None:
    global _reporter
    _reporter = r


async def report_loop(interval_s: float, reporter: Optional[OpsReporter] = None,
                      sleep: Callable[[float], Any] = asyncio.sleep) -> None:
    """Report now, then every interval_s seconds, until cancelled."""
    while True:
        try:
            await (reporter or await get_reporter()).run("schedule")
        except asyncio.CancelledError:
            raise
        except Exception as err:  # noqa: BLE001 - the loop must survive any single failure
            log.warning("ops report failed: %s", err)
        await sleep(interval_s)
