"""The HexaGo agent roster (PROJECT_CONTEXT.md section 3) and live registrations."""
from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Optional

from src.agents_py.core.events import EventStore, agent_id


@dataclass(frozen=True)
class AgentInfo:
    id: str
    name: str
    responsibility: str
    fallback_mode: str


ROSTER: List[AgentInfo] = [
    AgentInfo("booking_intent", "Booking Intent",
              "Parses natural language ride requests, extracts origin/destination/preferences, "
              "disambiguates vague drop-offs.", "Standard regex & structured form parser."),
    AgentInfo("dispatch_broker", "Dispatch Broker",
              "Matches rider demand to nearest drivers using H3 spatial clustering and driver preference scores.",
              "Classical nearest-neighbor greedy dispatch based on H3 distance."),
    AgentInfo("pool_batching", "Pool Batching",
              "Analyzes overlapping trip trajectories within 90-second batch windows for multi-rider pooling.",
              "First-come route detour threshold calculation (< 15% added ETA)."),
    AgentInfo("surge_equilibrium", "Surge Equilibrium",
              "Analyzes real-time supply/demand elasticity and live traffic to balance pricing.",
              "Fixed multiplier curve based on hex-cell supply/demand ratio."),
    AgentInfo("demand_forecast", "Demand Forecast",
              "Predicts hex-level ride requests 15-60 minutes ahead to preemptively position drivers.",
              "Historical moving average by day-of-week and hour bucket."),
    AgentInfo("safety_sentinel", "Safety Sentinel",
              "Continuously evaluates route anomalies, sudden stops, route deviations, and audio telemetry distress.",
              "Geofence deviation alert (> 500m off planned path for > 3 min)."),
    AgentInfo("compliance_vision", "Compliance Vision",
              "Audits vehicle inspection photos, driver documents, and regulatory roadworthiness.",
              "Manual hold queue + heuristic OCR verification."),
    AgentInfo("legal_jurisdiction", "Legal & Jurisdiction",
              "Adapts tariff regulations, municipal compliance rules, driver working-hour limits per jurisdiction.",
              "Static jurisdiction lookup table by municipal boundaries."),
    AgentInfo("settlement", "Settlement",
              "Computes driver earnings, platform take-rates, refunds, dynamic cancellation compensations.",
              "Deterministic distance + duration rate formula."),
    AgentInfo("support_dispute", "Support & Dispute",
              "Handles passenger complaints, lost-and-found items, and route dispute arbitration.",
              "Standard canned escalation workflow & credit issue matrix."),
    AgentInfo("ops_supervisor", "Ops Supervisor",
              "Monitors multi-agent health, detects reasoning drift, throttles rogue actions, "
              "and enforces platform guardrails.", "Automatic agent circuit breaker to non-LLM rule engines."),
]
ROSTER_BY_ID: Dict[str, AgentInfo] = {a.id: a for a in ROSTER}

# Agents whose decision logic already runs in another service.
EXTERNAL: Dict[str, str] = {"dispatch_broker": "dispatch-go", "safety_sentinel": "dispatch-go"}


@dataclass
class RegisteredAgent:
    id: str
    name: str
    description: str
    handler: Callable[..., Any]


_registered: Dict[str, RegisteredAgent] = {}


def register(name: str, description: str, handler: Callable[..., Any]) -> RegisteredAgent:
    aid = agent_id(name)
    display = ROSTER_BY_ID[aid].name if aid in ROSTER_BY_ID else name
    reg = RegisteredAgent(aid, display, description, handler)
    _registered[aid] = reg
    return reg


def registered() -> Dict[str, RegisteredAgent]:
    return dict(_registered)


def unregister(name: str) -> None:
    _registered.pop(agent_id(name), None)


def status_of(aid: str) -> str:
    return "active" if aid in _registered or aid in EXTERNAL else "planned"


async def list_agents(store: EventStore, now: Optional[dt.datetime] = None) -> List[Dict[str, Any]]:
    """The 11 roster agents with status and last-hour activity from the audit trail."""
    now = now or dt.datetime.now(dt.timezone.utc)
    stats = await store.agent_stats(now - dt.timedelta(hours=1))
    out = []
    for a in ROSTER:
        s = stats.get(a.id, {"count": 0, "fallbacks": 0, "last": None, "model": None})
        last = s["last"]
        reg = _registered.get(a.id)
        out.append({
            "id": a.id,
            "name": a.name,
            "responsibility": a.responsibility,
            "description": reg.description if reg else a.responsibility,
            "fallback_mode": a.fallback_mode,
            "status": status_of(a.id),
            "runs_in": "agents-py" if reg else EXTERNAL.get(a.id),
            "decisions_last_hour": s["count"],
            "fallback_rate": round(s["fallbacks"] / s["count"], 4) if s["count"] else 0.0,
            "last_model": s.get("model"),
            "last_decision": None if last is None else {
                "decision": last["decision"], "reasoning": last["reasoning"], "ts": last["ts"],
                "used_fallback": last["used_fallback"], "confidence": last["confidence"],
            },
        })
    return out
