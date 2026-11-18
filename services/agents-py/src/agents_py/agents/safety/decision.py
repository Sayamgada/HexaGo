"""Safety Sentinel decision rules: pure, deterministic, instant.

Nothing here does I/O or waits on an LLM. The sentinel decides with these
functions the moment a signal or a rider reply arrives; LLM output that lands
later can only *upgrade* a decision (see `upgrade_action`), never downgrade it.

    decide(severity, ctx)            signal      -> monitor | check_in | escalate
    classify_reply(text)             transcript  -> fine | distress | unclear
    decide_after_reply(cls)          rider reply -> monitor | escalate
    decide_on_timeout(timeout_s)     no reply    -> escalate
    escalation_allowed(times, now)   rate limit: 3 escalations per driver per hour
    resolution_violations(...)       the Ops Supervisor's rule-check before an incident closes
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple

MONITOR = "monitor"
CHECK_IN = "check_in"
ESCALATE = "escalate"
ACTIONS = (MONITOR, CHECK_IN, ESCALATE)
_RANK = {MONITOR: 0, CHECK_IN: 1, ESCALATE: 2}

ESCALATE_AT = 0.8  # signal severity that escalates without asking the rider
CHECK_IN_AT = 0.3  # below this the sentinel only keeps watching
HISTORY_WEIGHT = 0.05  # each prior incident of the driver (max 3) adds this much concern
MAX_ESCALATIONS_PER_HOUR = 3
RATE_WINDOW_S = 3600.0
CHECKIN_TIMEOUT_S = 30.0
CHECKIN_TIMEOUT_DEMO_S = 15.0
LLM_UPGRADE_CONFIDENCE = 0.6

FINE = "fine"
DISTRESS = "distress"
UNCLEAR = "unclear"


@dataclass(frozen=True)
class SignalContext:
    """What the sentinel knows about the ride when a signal arrives."""

    prior_incidents: int = 0
    prior_escalations: int = 0
    tier: str = ""
    ride_status: str = ""


@dataclass(frozen=True)
class Decision:
    action: str
    reason: str
    severity: float  # effective severity (signal + history)
    signal_severity: float = 0.0
    suppressed: bool = False  # an escalation the rate limit turned into monitoring


def clamp(v: float, lo: float = 0.0, hi: float = 1.0) -> float:
    return max(lo, min(hi, float(v)))


def effective_severity(severity: float, ctx: SignalContext = SignalContext()) -> float:
    """Signal severity raised a little for drivers with a recent incident history."""
    return round(clamp(clamp(severity) + HISTORY_WEIGHT * min(max(ctx.prior_incidents, 0), 3)), 3)


def decide(severity: float, ctx: SignalContext = SignalContext()) -> Decision:
    """The first decision on a safety signal."""
    sev = clamp(severity)
    eff = effective_severity(sev, ctx)
    if sev >= ESCALATE_AT:
        return Decision(ESCALATE, f"severity {sev:.2f} >= {ESCALATE_AT}: escalating immediately", eff, sev)
    if eff >= ESCALATE_AT:
        return Decision(ESCALATE, f"severity {sev:.2f} plus {ctx.prior_incidents} prior incident(s) "
                                  f"reaches {eff:.2f}: escalating", eff, sev)
    if eff >= CHECK_IN_AT:
        return Decision(CHECK_IN, f"severity {eff:.2f}: calling the rider to check in", eff, sev)
    return Decision(MONITOR, f"severity {eff:.2f} < {CHECK_IN_AT}: monitoring the trip", eff, sev)


# ---- rider replies ----

# Distress wins over reassurance: "I'm not ok" contains "ok", "please stop" contains no "fine".
_DISTRESS = (
    r"help", r"scared", r"afraid", r"frightened", r"terrified", r"stop", r"police", r"unsafe", r"hurt",
    r"emergency", r"danger(?:ous)?", r"attack(?:ed|ing)?", r"kidnap\w*", r"sos", r"bleed\w*", r"injur\w*",
    r"accident", r"crash(?:ed)?", r"threat\w*", r"weapon", r"knife", r"gun", r"drunk", r"harass\w*",
    r"not (?:ok|okay|fine|safe|good|alright|all right)", r"don'?t feel (?:safe|good|ok|okay)",
    r"get me out", r"let me out", r"call (?:the )?(?:police|someone|ambulance)", r"ambulance", r"no+$",
)
_FINE = (
    r"(?:i'?m |i am |we'?re |all )?(?:fine|ok|okay|good|alright|all right|safe)",
    r"no problem", r"nothing (?:is )?wrong", r"false alarm", r"yes", r"yeah", r"yep", r"sure",
    r"all good", r"no worries", r"just (?:a )?(?:pothole|speed ?bump|detour|traffic)",
)
_DISTRESS_RE = re.compile(r"\b(?:" + "|".join(_DISTRESS) + r")\b")
_FINE_RE = re.compile(r"\b(?:" + "|".join(_FINE) + r")\b")


@dataclass(frozen=True)
class ReplyClass:
    label: str
    matched: Tuple[str, ...] = ()
    confidence: float = 0.0


def normalize(text: Optional[str]) -> str:
    t = (text or "").lower().replace("’", "'").replace("‘", "'")
    return re.sub(r"\s+", " ", re.sub(r"[^a-z0-9' ]+", " ", t)).strip()


def classify_reply(text: Optional[str]) -> ReplyClass:
    """Keyword classifier for a rider's spoken or typed answer."""
    t = normalize(text)
    if not t:
        return ReplyClass(UNCLEAR, (), 0.0)
    distress = tuple(m.group(0) for m in _DISTRESS_RE.finditer(t))
    if distress:
        return ReplyClass(DISTRESS, distress, 0.9)
    fine = tuple(m.group(0) for m in _FINE_RE.finditer(t))
    if fine:
        return ReplyClass(FINE, fine, 0.8)
    return ReplyClass(UNCLEAR, (), 0.3)


def decide_after_reply(cls: ReplyClass, severity: float = 0.0) -> Decision:
    """Act on the rider's answer: "I'm fine" -> monitor; distress or no clear answer -> escalate."""
    sev = clamp(severity)
    if cls.label == FINE:
        return Decision(MONITOR, f"rider confirmed they are safe ({', '.join(cls.matched)})", sev, sev)
    if cls.label == DISTRESS:
        return Decision(ESCALATE, f"rider reported distress ({', '.join(cls.matched)})", max(sev, ESCALATE_AT), sev)
    return Decision(ESCALATE, "rider's reply did not confirm they are safe", max(sev, ESCALATE_AT), sev)


def decide_on_timeout(timeout_s: float, severity: float = 0.0) -> Decision:
    sev = clamp(severity)
    return Decision(ESCALATE, f"no reply from the rider within {timeout_s:g} s", max(sev, ESCALATE_AT), sev)


DEVIATION_THRESHOLD_M = 300.0  # dispatch-go detector: off the planned polyline by more than this


def deviation_threshold_m() -> float:
    return DEVIATION_THRESHOLD_M


def checkin_timeout_s(demo: bool) -> float:
    return CHECKIN_TIMEOUT_DEMO_S if demo else CHECKIN_TIMEOUT_S


def rank(action: str) -> int:
    return _RANK.get(action, -1)


def upgrade_action(current: str, llm: Dict[str, Any]) -> Optional[str]:
    """The action an async LLM classification asks for, if it is an upgrade.

    Returns ``ESCALATE`` only when the model is confident the rider is in
    distress and the current action is lower; anything else returns None.
    LLM output can never downgrade a decision.
    """
    try:
        distress = bool(llm.get("distress"))
        confidence = float(llm.get("confidence") or 0)
    except (AttributeError, TypeError, ValueError):
        return None
    if distress and confidence >= LLM_UPGRADE_CONFIDENCE and rank(current) < rank(ESCALATE):
        return ESCALATE
    return None


def escalation_allowed(times: Iterable[float], now: float, limit: int = MAX_ESCALATIONS_PER_HOUR,
                       window_s: float = RATE_WINDOW_S) -> bool:
    """At most `limit` escalations per driver in any `window_s` seconds."""
    return sum(1 for t in times if now - window_s < t <= now) < limit


def apply_rate_limit(decision: Decision, times: Iterable[float], now: float, rider_confirmed: bool = False) -> Decision:
    """Turn an over-limit automatic escalation into monitoring (a suppressed likely false positive).

    An escalation the rider asked for (distress in their reply) is never suppressed.
    """
    if decision.action != ESCALATE or rider_confirmed or escalation_allowed(times, now):
        return decision
    return Decision(MONITOR, f"escalation suppressed: driver already had {MAX_ESCALATIONS_PER_HOUR} escalations "
                             f"in the last hour (likely false positive; was: {decision.reason})",
                    decision.severity, decision.signal_severity, suppressed=True)


# ---- wording ----

_TRIGGER_WORDS = {
    "route_deviation": "left the planned route",
    "harsh_deceleration": "braked very hard",
    "unexpected_stop": "stopped unexpectedly",
    "speed_anomaly": "was driving unusually fast",
}


def describe_triggers(triggers: Sequence[Dict[str, Any]]) -> str:
    words: List[str] = []
    for t in triggers or ():
        w = _TRIGGER_WORDS.get(str(t.get("type")))
        if w and w not in words:
            words.append(w)
    if not words:
        return "reported an anomaly"
    if len(words) == 1:
        return words[0]
    return ", ".join(words[:-1]) + " and " + words[-1]


def checkin_question(triggers: Sequence[Dict[str, Any]]) -> str:
    return (f"Hi, this is HexaGo Safety. We noticed your vehicle {describe_triggers(triggers)}. "
            "Are you okay? Say \"I'm fine\" if everything is alright, or \"help\" if you need assistance.")


def template_reasoning(signal: Dict[str, Any], decision: Decision, ctx: SignalContext = SignalContext()) -> str:
    """Deterministic reasoning text shown until (or instead of) the LLM's."""
    parts = [f"Telemetry shows the vehicle {describe_triggers(signal.get('triggers') or [])} "
             f"(signal severity {decision.signal_severity:.2f})."]
    if ctx.prior_incidents:
        parts.append(f"The driver has {ctx.prior_incidents} earlier incident(s).")
    parts.append({
        ESCALATE: "Rule: escalate - emergency services, live location sharing and driver suspension.",
        CHECK_IN: "Rule: call the rider and act on their answer; no answer escalates.",
        MONITOR: "Rule: keep monitoring the trip; no intervention.",
    }[decision.action])
    if decision.suppressed:
        parts.append("Escalation suppressed by the per-driver rate limit.")
    return " ".join(parts)


# ---- Ops Supervisor rule-check ----

SAFETY_AGENT = "safety_sentinel"
SUPERVISOR = "ops_supervisor"


@dataclass(frozen=True)
class Confirmation:
    """The Ops Supervisor's verdict on closing an incident."""

    incident_id: str
    approved: bool
    agent: str = SUPERVISOR
    violations: Tuple[str, ...] = field(default_factory=tuple)

    @property
    def valid(self) -> bool:
        return self.approved and self.agent == SUPERVISOR and not self.violations


def resolution_violations(incident: Dict[str, Any], requested_by: str, note: str,
                          pending_checkins: int = 0) -> List[str]:
    """Why an incident may not be closed yet (empty list = may close)."""
    out: List[str] = []
    who = (requested_by or "").strip()
    if not who:
        out.append("requested_by is required")
    elif who == SAFETY_AGENT:
        out.append("the Safety Sentinel cannot close an incident or unfreeze a driver by itself")
    if incident.get("status") == "resolved":
        out.append("incident is already resolved")
    if pending_checkins:
        out.append("a rider check-in is still waiting for an answer")
    if len((note or "").strip()) < 5:
        out.append("a resolution note (at least 5 characters) is required")
    return out
