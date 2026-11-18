"""Ops Supervisor heartbeat: one deterministic summary event every few minutes.

No LLM: it reads the audit trail and the LLM budget and reports swarm health,
so the Ops feed always shows activity even when nothing else is happening.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import logging
import time
from typing import Any, Dict, Optional

from src.agents_py.agents.safety.decision import Confirmation, resolution_violations
from src.agents_py.core.events import EventStore, get_event_store
from src.agents_py.core.graph import agent
from src.agents_py.core.llm import LLMClient, get_llm

log = logging.getLogger("hexago.ops_supervisor")

AGENT = "ops_supervisor"
HIGH_FALLBACK_RATE = 0.5
MIN_DECISIONS_FOR_ALERT = 10


@agent(AGENT, "Monitors swarm health; emits a deterministic heartbeat summary every 5 minutes.")
async def heartbeat(store: Optional[EventStore] = None, llm: Optional[LLMClient] = None,
                    now: Optional[dt.datetime] = None) -> Dict[str, Any]:
    start = time.perf_counter()
    store = store or await get_event_store()
    llm = llm or await get_llm()
    now = now or dt.datetime.now(dt.timezone.utc)
    stats = await store.agent_stats(now - dt.timedelta(hours=1))
    others = {k: v for k, v in stats.items() if k != AGENT}
    total = sum(s["count"] for s in others.values())
    fallbacks = sum(s["fallbacks"] for s in others.values())
    rate = fallbacks / total if total else 0.0
    active = sorted(k for k, s in others.items() if s["count"])
    budget = await llm.budget()
    calls, call_limit = budget["calls_today"], budget["limits"]["daily_calls"]

    concerns = []
    if total >= MIN_DECISIONS_FOR_ALERT and rate >= HIGH_FALLBACK_RATE:
        concerns.append(f"fallback rate {rate:.0%}")
    if budget["enabled"] and calls >= call_limit:
        concerns.append("LLM daily call budget exhausted")
    decision = "heartbeat: attention needed" if concerns else "heartbeat: swarm healthy"
    reasoning = (
        f"{total} decisions in the last hour from {len(active)} agent(s)"
        f"{' (' + ', '.join(active) + ')' if active else ''}; fallback rate {rate:.0%}; "
        f"LLM {calls}/{call_limit} calls, {budget['tokens_today']} tokens today"
        f"{'; LLM disabled' if not budget['enabled'] else ''}"
        f"{'; ' + '; '.join(concerns) if concerns else ''}."
    )
    return await store.record_event(
        AGENT, "periodic heartbeat", decision, reasoning, 1.0,
        (time.perf_counter() - start) * 1000, False, None,
        {"kind": "heartbeat", "decisions_last_hour": total, "fallback_rate": round(rate, 4),
         "llm_calls_today": calls},
    )


async def review_resolution(incident: Dict[str, Any], *, requested_by: str, note: str, pending_checkins: int = 0,
                            store: Optional[EventStore] = None) -> Confirmation:
    """Guardrail: rule-check a request to close a safety incident (and unfreeze its driver).

    Deterministic (no LLM). The Safety Sentinel can never close its own
    incidents; a pending rider check-in or a missing resolution note blocks
    the close. The verdict is written to agent_events either way.
    """
    start = time.perf_counter()
    store = store or await get_event_store()
    violations = resolution_violations(incident, requested_by, note, pending_checkins)
    conf = Confirmation(incident_id=str(incident.get("id")), approved=not violations, violations=tuple(violations))
    verdict = "confirmed" if conf.valid else "rejected"
    await store.record_event(
        AGENT, f"resolve request for incident {incident.get('id')} by {requested_by or '?'}",
        f"incident {incident.get('id')} resolution {verdict}",
        ("All guardrail checks passed: driver may return to service." if conf.valid
         else "Guardrail checks failed: " + "; ".join(violations) + "."),
        1.0, (time.perf_counter() - start) * 1000, False, None,
        {"kind": "safety_resolution_review", "incident_id": incident.get("id"), "ride_id": incident.get("ride_id"),
         "driver_id": incident.get("driver_id"), "approved": conf.valid, "violations": violations},
    )
    return conf


async def heartbeat_loop(interval_s: float) -> None:
    """Beat now, then every interval_s seconds, until cancelled."""
    while True:
        try:
            await heartbeat()
        except asyncio.CancelledError:
            raise
        except Exception as err:  # noqa: BLE001 - the loop must survive any single failure
            log.warning("heartbeat failed: %s", err)
        await asyncio.sleep(interval_s)
