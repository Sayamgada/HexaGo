"""Settlement agent: a periodic, deterministic audit of the dispatch-go ledger.

Every ``SETTLEMENT_SCAN_INTERVAL_S`` (60 s) it reads recent journal entries
and payouts from dispatch-go, runs the pure checks in ``checks.py`` and logs
the run to ``agent_events``. The LLM never decides anything: for each newly
flagged item it only writes a human explanation, asynchronously, through
``core.llm`` (guardrails, budget, cache). Until that lands - or when the LLM
is unavailable - the finding carries its deterministic template text.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import logging
import time
from collections import OrderedDict
from typing import Any, Callable, Dict, List, Optional, Set

import httpx
from pydantic import BaseModel, Field

from src.agents_py.agents.settlement.checks import Finding, run_checks
from src.agents_py.config import settings
from src.agents_py.core.events import EventStore, get_event_store
from src.agents_py.core.llm import LLMClient, get_llm

log = logging.getLogger("hexago.settlement")

AGENT = "settlement"
WINDOW_S = 900            # entries and payouts of the last 15 minutes
MAX_EXPLANATIONS = 3      # new LLM explanations per run (the rest keep the template)
MAX_FINDINGS = 200        # findings remembered across runs


class Explanation(BaseModel):
    explanation: str = Field(..., max_length=600)
    recommended_action: str = Field("", max_length=300)


class LedgerError(Exception):
    pass


class LedgerClient:
    """GET /v1/ledger/entries and /v1/ledger/payouts on dispatch-go."""

    def __init__(self, base_url: Optional[str] = None, timeout_s: float = 5.0,
                 transport: Optional[httpx.AsyncBaseTransport] = None):
        self._http = httpx.AsyncClient(base_url=(base_url or settings.DISPATCH_URL).rstrip("/"), timeout=timeout_s,
                                       transport=transport)

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _get(self, path: str, params: Dict[str, Any]) -> Dict[str, Any]:
        try:
            resp = await self._http.get(path, params=params)
        except httpx.HTTPError as err:
            raise LedgerError(f"GET {path}: {err}") from err
        if resp.status_code >= 400:
            raise LedgerError(f"GET {path}: HTTP {resp.status_code}: {resp.text[:200]}")
        return resp.json()

    async def entries(self, since: dt.datetime) -> List[Dict[str, Any]]:
        out = await self._get("/v1/ledger/entries", {"since": _rfc3339(since), "limit": 1000})
        return out.get("entries") or []

    async def payouts(self, **params: Any) -> List[Dict[str, Any]]:
        if isinstance(params.get("since"), dt.datetime):
            params["since"] = _rfc3339(params["since"])
        out = await self._get("/v1/ledger/payouts", {"limit": 1000, **params})
        return out.get("payouts") or []


def _rfc3339(t: dt.datetime) -> str:
    return t.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _ms(start: float) -> float:
    return (time.perf_counter() - start) * 1000


class SettlementAgent:
    def __init__(self, client: Optional[LedgerClient] = None, store: Optional[EventStore] = None,
                 llm: Optional[LLMClient] = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)):
        self.client = client or LedgerClient()
        self._store, self._llm = store, llm
        self.clock = clock
        self.findings: "OrderedDict[str, Dict[str, Any]]" = OrderedDict()  # key -> finding view
        self.last_run: Optional[Dict[str, Any]] = None
        self.runs = 0
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
        """Wait for pending explanation tasks (tests)."""
        while self._tasks:
            await asyncio.gather(*list(self._tasks), return_exceptions=True)

    async def scan(self) -> Dict[str, Any]:
        """One audit run: fetch, check, log; new findings get an async LLM explanation."""
        start = time.perf_counter()
        now = self.clock()
        since = now - dt.timedelta(seconds=WINDOW_S)
        store = await self.store()
        try:
            entries = await self.client.entries(since)
            payouts = {p["id"]: p for p in await self.client.payouts(since=since)}
            for status in ("held_for_review", "pending"):  # any age
                payouts.update({p["id"]: p for p in await self.client.payouts(status=status)})
        except LedgerError as err:
            self.last_run = {"at": now.isoformat(), "ok": False, "error": str(err)}
            await store.record_event(AGENT, "ledger scan", "ledger scan skipped: dispatch-go unreachable", str(err),
                                     0.0, _ms(start), True, "error", {"kind": "settlement_scan", "error": str(err)[:300]})
            return self.last_run

        found = run_checks(entries, list(payouts.values()), now)
        new = [f for f in found if f.key not in self.findings]
        for f in found:
            view = self.findings.pop(f.key, None) or {**f.to_dict(), "explanation": f.template(),
                                                       "explanation_source": "template", "first_seen": now.isoformat()}
            view["last_seen"] = now.isoformat()
            self.findings[f.key] = view
        while len(self.findings) > MAX_FINDINGS:
            self.findings.popitem(last=False)
        for f in new[:MAX_EXPLANATIONS]:
            self._spawn(self._explain(f))

        self.runs += 1
        by_check: Dict[str, int] = {}
        for f in found:
            by_check[f.check] = by_check.get(f.check, 0) + 1
        rides = sum(1 for e in entries if e.get("kind") == "ride_settlement")
        decision = (f"ledger scan: {len(found)} anomal{'y' if len(found) == 1 else 'ies'} flagged" if found
                    else "ledger scan: no anomalies")
        reasoning = (f"Checked {len(entries)} entries ({rides} ride settlements) and {len(payouts)} payouts from the "
                     f"last {WINDOW_S // 60} min: split vs formula, balance, refund loops, unusual tips, payout delay "
                     f"> 5 s, holds > 10 min."
                     + (" Flagged: " + ", ".join(f"{k} x{v}" for k, v in sorted(by_check.items())) + "." if found else ""))
        self.last_run = {"at": now.isoformat(), "ok": True, "entries": len(entries), "payouts": len(payouts),
                         "rides": rides, "flagged": len(found), "new": len(new), "by_check": by_check}
        await store.record_event(
            AGENT, f"ledger scan ({len(entries)} entries, {len(payouts)} payouts)", decision, reasoning, 1.0,
            _ms(start), False, None,
            {"kind": "settlement_scan", **{k: v for k, v in self.last_run.items() if k not in ("at", "ok")},
             "findings": [self.findings.get(f.key) or f.to_dict() for f in found[:10]]},
        )
        return self.last_run

    def _spawn(self, coro) -> None:
        task = asyncio.create_task(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def _explain(self, f: Finding) -> None:
        start = time.perf_counter()
        prompt = (
            "You are HexaGo's Settlement agent auditing a double-entry ride-hailing ledger (amounts in INR). "
            "A deterministic check flagged the item below. Explain in two sentences, for an operations engineer, "
            "what most likely happened and the next step. Do not invent numbers beyond the facts.\n"
            f"check: {f.check}\nride: {f.ride_id}\ndriver: {f.driver_id}\ndetail: {f.detail}\nfacts: {f.facts}"
        )
        try:
            res = await (await self.llm()).complete_json(
                prompt, Explanation, AGENT,
                lambda: {"explanation": f.template(), "recommended_action": ""})
        except Exception as err:  # noqa: BLE001 - an explanation must never break the agent
            log.warning("settlement explanation failed: %s", err)
            return
        view = self.findings.get(f.key)
        if view is not None:
            view["explanation"] = res.data["explanation"]
            view["recommended_action"] = res.data.get("recommended_action", "")
            view["explanation_source"] = "template" if res.used_fallback else "llm"
        store = await self.store()
        await store.record_event(
            AGENT, f"explain {f.check} on ride {f.ride_id}", f"settlement anomaly: {f.check} on ride {f.ride_id[:8]}",
            res.data["explanation"], f.severity, _ms(start), res.used_fallback, res.fallback_reason,
            {"kind": "settlement_anomaly", "ride_id": f.ride_id, "driver_id": f.driver_id, "check": f.check,
             "key": f.key, "tokens": res.tokens, "model": res.model},
        )

    def view(self) -> Dict[str, Any]:
        return {"last_run": self.last_run, "runs": self.runs, "findings": list(reversed(self.findings.values()))}


_agent: Optional[SettlementAgent] = None


def get_agent() -> SettlementAgent:
    global _agent
    if _agent is None:
        _agent = SettlementAgent()
    return _agent


def set_agent(a: Optional[SettlementAgent]) -> None:
    global _agent
    _agent = a


async def scan_loop(interval_s: float, agent: Optional[SettlementAgent] = None,
                    sleep: Callable[[float], Any] = asyncio.sleep) -> None:
    """Scan now, then every interval_s seconds, until cancelled."""
    while True:
        try:
            await (agent or get_agent()).scan()
        except asyncio.CancelledError:
            raise
        except Exception as err:  # noqa: BLE001 - the loop must survive any single failure
            log.warning("settlement scan failed: %s", err)
        await sleep(interval_s)
