"""Safety Sentinel: autonomous incident response for a ride in progress.

A LangGraph flow runs on every ``safety_signal`` from dispatch-go's detector:

    gather_context (ride, driver history, previous incidents, location)
      -> decide (pure `decision.decide`, severity >= 0.8 escalates at once)
      -> act    (monitor | check_in | escalate)

The decision never waits on the LLM. LLM calls (reasoning text for the
incident, classification of the rider's reply) run as background tasks
through `core.llm` with its guardrails; until they land the incident shows
template reasoning, and an LLM verdict can only upgrade an action.

check_in  opens a call session and pushes a ``safety_call`` frame on
          ``ride:<id>`` and ``ops``; the rider answers through
          POST /v1/safety/checkins/{id}/reply. "I'm fine" -> monitor,
          distress or an unclear answer -> escalate, silence for 30 s
          (15 s in DEMO_MODE) -> escalate.
escalate  calls the simulated emergency dispatch, shares the live location
          (/track/{token}), suspends the driver through dispatch-go (which
          stops the vehicle and re-dispatches an AV rider), notifies the
          rider's emergency contact and tells the rider help is coming.

Guardrails: the sentinel can never unfreeze a driver or close an incident
itself (`resolve` needs the Ops Supervisor's approved confirmation, which
`SafetyStore.close_incident` and `SafetyActions.unsuspend_driver` both
enforce); at most 3 automatic escalations per driver per hour, the rest are
logged as suppressed false positives. Every step is a timeline entry and an
``agent_events`` row.
"""
from __future__ import annotations

import asyncio
import datetime as dt
import hashlib
import json
import logging
import time
from typing import Any, Awaitable, Callable, Dict, List, Optional, Set, TypedDict

from langgraph.graph import END, START, StateGraph
from pydantic import BaseModel, Field

from src.agents_py.agents.safety import decision as rules
from src.agents_py.agents.safety.actions import ActionError, SafetyActions
from src.agents_py.agents.safety.decision import Confirmation, Decision, SignalContext
from src.agents_py.agents.safety.store import SafetyStore, public
from src.agents_py.config import settings
from src.agents_py.core import events
from src.agents_py.core.llm import get_llm

log = logging.getLogger("hexago.safety")

AGENT = "safety_sentinel"
CHECKIN_AGENT = "safety_sentinel_checkin"  # own LLM min-interval bucket: a reply is a separate meaningful event
DETECTOR = "safety_detector"
RIDER = "rider"
SUPERVISOR = rules.SUPERVISOR
CONTEXT_TIMEOUT_S = 2.0


class Reasoning(BaseModel):
    reasoning: str = Field(..., max_length=600)
    risk: str = "unknown"


class ReplyAssessment(BaseModel):
    distress: bool
    sentiment: str = "neutral"
    confidence: float = Field(0.0, ge=0.0, le=1.0)


class CheckinNotFound(KeyError):
    pass


class CheckinClosed(Exception):
    def __init__(self, checkin: Dict[str, Any]):
        super().__init__(f"check-in is {checkin.get('status')}")
        self.checkin = checkin


class IncidentNotFound(KeyError):
    pass


class ResolutionRejected(Exception):
    def __init__(self, confirmation: Confirmation):
        super().__init__("; ".join(confirmation.violations) or "not confirmed by the Ops Supervisor")
        self.confirmation = confirmation


class SentinelState(TypedDict, total=False):
    signal: Dict[str, Any]
    ride: Optional[Dict[str, Any]]
    history: List[Dict[str, Any]]
    context: SignalContext
    decision: Decision
    output: Dict[str, Any]
    started: float


Supervisor = Callable[..., Awaitable[Confirmation]]


def emergency_contact(rider_id: str) -> Dict[str, str]:
    """The rider's (simulated) emergency contact."""
    digits = int(hashlib.sha256((rider_id or "rider").encode()).hexdigest(), 16) % 100000
    return {"name": f"Emergency contact of {rider_id or 'the rider'}", "phone": f"+91 90000 {digits:05d}",
            "relation": "simulated"}


def _ms(start: float) -> float:
    return round((time.perf_counter() - start) * 1000, 3)


class SafetySentinel:
    def __init__(self, store: Optional[SafetyStore] = None, actions: Optional[SafetyActions] = None, *,
                 llm: Any = None, record: Optional[Callable[..., Awaitable[Any]]] = None,
                 supervisor: Optional[Supervisor] = None, demo: Optional[bool] = None,
                 checkin_timeout_s: Optional[float] = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc),
                 sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
                 context_timeout_s: float = CONTEXT_TIMEOUT_S):
        self.store = store or SafetyStore(clock=clock)
        self.actions = actions or SafetyActions()
        self._llm = llm
        self._record = record or events.record_event
        if supervisor is None:
            from src.agents_py.agents.ops_supervisor import review_resolution
            supervisor = review_resolution
        self._supervisor = supervisor
        self.demo = settings.DEMO_MODE if demo is None else demo
        self.checkin_timeout_s = float(checkin_timeout_s or rules.checkin_timeout_s(self.demo))
        self.clock = clock
        self.sleep = sleep
        self.context_timeout_s = context_timeout_s
        self._tasks: Set[asyncio.Task] = set()
        self._timers: Dict[str, asyncio.Task] = {}
        self._locks: Dict[str, asyncio.Lock] = {}
        self.graph = self._build_graph()

    # ---- plumbing ----

    def spawn(self, coro: Awaitable[Any], name: str = "safety") -> asyncio.Task:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)

        def done(t: asyncio.Task) -> None:
            self._tasks.discard(t)
            if not t.cancelled() and t.exception() is not None:
                log.warning("%s task failed: %r", name, t.exception())

        task.add_done_callback(done)
        return task

    async def idle(self) -> None:
        """Wait until every background task (LLM, timers, escalations) finished (tests, shutdown)."""
        while self._tasks:
            await asyncio.gather(*list(self._tasks), return_exceptions=True)

    async def llm(self) -> Any:
        return self._llm if self._llm is not None else await get_llm()

    def _now_s(self) -> float:
        return self.clock().timestamp()

    async def _event(self, decision: str, reasoning: str, *, kind: str, inc: Optional[Dict[str, Any]] = None,
                     signal: Optional[Dict[str, Any]] = None, agent: str = AGENT, confidence: float = 1.0,
                     latency_ms: float = 0.0, used_fallback: bool = False, fallback_reason: Optional[str] = None,
                     summary: str = "", **meta: Any) -> None:
        src = inc or signal or {}
        metadata = {"kind": kind, "ride_id": src.get("ride_id") or "", "driver_id": src.get("driver_id") or "",
                    **({"incident_id": inc["id"]} if inc else {}), **meta}
        try:
            await self._record(agent, summary or kind, decision, reasoning, confidence, latency_ms, used_fallback,
                               fallback_reason, public(metadata))
        except Exception as err:  # noqa: BLE001 - the audit write must never stop the response
            log.warning("record safety event failed: %s", err)

    async def _step(self, inc: Dict[str, Any], step: str, actor: str, decision: str, why: str = "",
                    *, event: bool = True, used_fallback: bool = False, fallback_reason: Optional[str] = None,
                    confidence: float = 1.0, latency_ms: float = 0.0, **details: Any) -> Dict[str, Any]:
        """One incident step: a timeline entry plus an agent_events row."""
        entry = await self.store.add_step(inc["id"], step, actor, summary=decision, **details)
        if event:
            await self._event(decision, why or decision, kind=f"safety_{step}", inc=inc,
                              agent=actor if actor in (AGENT, SUPERVISOR) else AGENT, confidence=confidence,
                              latency_ms=latency_ms, used_fallback=used_fallback, fallback_reason=fallback_reason,
                              summary=f"{step} ({actor})", step=step)
        return entry

    def _lock(self, incident_id: str) -> asyncio.Lock:
        return self._locks.setdefault(incident_id, asyncio.Lock())

    # ---- graph ----

    def _build_graph(self):
        g = StateGraph(SentinelState)
        g.add_node("gather_context", self._gather)
        g.add_node("decide", self._decide)
        g.add_node("act", self._act)
        g.add_edge(START, "gather_context")
        g.add_edge("gather_context", "decide")
        g.add_edge("decide", "act")
        g.add_edge("act", END)
        return g.compile()

    async def handle_signal(self, signal: Dict[str, Any]) -> Dict[str, Any]:
        """Decide on a safety signal now; follow-up work runs in the background."""
        state = await self.graph.ainvoke({"signal": dict(signal), "started": time.perf_counter()})
        return state["output"]

    async def _gather(self, state: SentinelState) -> Dict[str, Any]:
        sig = state["signal"]

        async def ride() -> Optional[Dict[str, Any]]:
            if not sig.get("ride_id"):
                return None
            try:
                return await asyncio.wait_for(self.actions.get_ride(str(sig["ride_id"])), self.context_timeout_s)
            except (ActionError, asyncio.TimeoutError) as err:
                log.info("ride context for %s unavailable: %s", sig.get("ride_id"), err)
                return None

        ride_info, history = await asyncio.gather(ride(), self.store.incidents_for_driver(str(sig.get("driver_id") or "")))
        ctx = SignalContext(
            prior_incidents=len(history),
            prior_escalations=sum(1 for h in history if h.get("status") in ("escalated", "resolved")
                                  and (h.get("actions") or {}).get("escalated_at")),
            tier=str(sig.get("tier") or (ride_info or {}).get("tier") or ""),
            ride_status=str((ride_info or {}).get("status") or ""),
        )
        return {"ride": ride_info, "history": history, "context": ctx}

    async def _decide(self, state: SentinelState) -> Dict[str, Any]:
        sig = state["signal"]
        d = rules.decide(float(sig.get("severity") or 0), state["context"])
        if d.action == rules.ESCALATE:
            d = rules.apply_rate_limit(d, self.store.escalation_times(str(sig.get("driver_id") or "")), self._now_s())
        return {"decision": d}

    async def _act(self, state: SentinelState) -> Dict[str, Any]:
        sig, d, ctx = state["signal"], state["decision"], state["context"]
        latency = _ms(state["started"])
        if d.action == rules.MONITOR:
            if d.suppressed:
                self.store.record_suppressed({"signal_id": sig.get("signal_id"), "ride_id": sig.get("ride_id"),
                                              "driver_id": sig.get("driver_id"), "severity": d.signal_severity,
                                              "reason": d.reason})
                await self._event(f"suppressed escalation for {sig.get('driver_id')} on ride {sig.get('ride_id')} "
                                  "(likely false positive)", d.reason, kind="safety_suppressed", signal=sig,
                                  confidence=d.severity, latency_ms=latency)
            else:
                await self._event(f"monitoring ride {sig.get('ride_id')}: {d.reason}",
                                  rules.template_reasoning(sig, d, ctx), kind="safety_monitor", signal=sig,
                                  confidence=1 - d.severity, latency_ms=latency)
            return {"output": {"action": rules.MONITOR, "suppressed": d.suppressed, "reason": d.reason,
                               "incident_id": None, "severity": d.severity, "latency_ms": latency}}

        status = "check_in" if d.action == rules.CHECK_IN else "escalated"
        ride = state.get("ride") or {}
        inc = await self.store.create_incident(
            ride_id=sig.get("ride_id"), driver_id=sig.get("driver_id"), rider_id=sig.get("rider_id") or ride.get("rider_id"),
            tier=ctx.tier, signal_id=sig.get("signal_id"), severity=d.severity, signal_severity=d.signal_severity,
            status=status, decision=d.action, reason=d.reason, reasoning=rules.template_reasoning(sig, d, ctx),
            triggers=list(sig.get("triggers") or []), location=sig.get("location"),
            planned_route=list(sig.get("planned_route") or []), trail=list(sig.get("trail") or []),
            pickup=sig.get("pickup") or ride.get("pickup"), dropoff=sig.get("dropoff") or ride.get("dropoff"),
            detected_at=sig.get("detected_at"),
        )
        trig = rules.describe_triggers(inc["triggers"])
        await self._step(inc, "signal_received", DETECTOR, f"safety signal on ride {inc['ride_id']}: vehicle {trig}",
                         severity=d.signal_severity, triggers=inc["triggers"], location=inc["location"],
                         signal_id=inc["signal_id"])
        await self._step(inc, "context_gathered", AGENT,
                         f"context: {ctx.prior_incidents} prior incident(s), ride {ctx.ride_status or 'unknown'}, "
                         f"tier {ctx.tier or 'unknown'}", prior_incidents=ctx.prior_incidents,
                         prior_escalations=ctx.prior_escalations, ride_status=ctx.ride_status, tier=ctx.tier)
        dstep = await self._step(inc, "decision", AGENT, f"{d.action}: {d.reason}", inc["reasoning"],
                                 confidence=d.severity, latency_ms=latency, action=d.action, severity=d.severity,
                                 reasoning=inc["reasoning"], reasoning_source="template")
        self.spawn(self._reason(inc["id"], sig, d, ctx, dstep["id"]), "reasoning")
        out: Dict[str, Any] = {"action": d.action, "incident_id": inc["id"], "severity": d.severity,
                               "reason": d.reason, "latency_ms": latency, "suppressed": False}
        if d.action == rules.CHECK_IN:
            chk = await self._start_checkin(inc)
            out["checkin_id"] = chk["id"]
        else:
            self.store.record_escalation(str(inc["driver_id"] or ""), self._now_s())
            self.spawn(self._escalate(inc["id"], d.reason), "escalate")
        return {"output": out}

    # ---- LLM (background, guarded, never blocking a decision) ----

    async def _reason(self, incident_id: str, sig: Dict[str, Any], d: Decision, ctx: SignalContext,
                      step_id: str) -> None:
        template = rules.template_reasoning(sig, d, ctx)
        prompt = (
            "You are the Safety Sentinel of a ride-hailing platform. A deterministic rule engine already "
            f"decided to {d.action.replace('_', ' ')} for this ride; explain the risk in at most two sentences "
            "for the operations log. Do not change the decision.\n"
            f"Telemetry triggers: {json.dumps(sig.get('triggers') or [], default=str)[:800]}\n"
            f"Signal severity: {d.signal_severity:.2f}; prior incidents of the driver: {ctx.prior_incidents}; "
            f"vehicle tier: {ctx.tier or 'unknown'}; decision reason: {d.reason}."
        )
        llm = await self.llm()
        res = await llm.complete_json(prompt, Reasoning, AGENT, lambda: {"reasoning": template, "risk": "unknown"})
        inc = self.store.incidents.get(incident_id)
        if inc is None:
            return
        if res.used_fallback:
            source = f"template ({res.fallback_reason})"
            await self.store.update_step(incident_id, step_id, reasoning_source=source)
            await self.store.update_incident(incident_id, reasoning_source=source)
            await self._event(f"reasoning for incident {incident_id}: template kept ({res.fallback_reason})", template,
                              kind="safety_reasoning", inc=inc, used_fallback=True,
                              fallback_reason=res.fallback_reason, latency_ms=res.latency_ms)
            return
        text = str(res.data.get("reasoning") or template)
        await self.store.update_step(incident_id, step_id, reasoning=text, reasoning_source="llm", model=res.model)
        await self.store.update_incident(incident_id, reasoning=text, reasoning_source="llm",
                                         risk=res.data.get("risk"))
        await self._event(f"reasoning for incident {incident_id} ({res.model})", text, kind="safety_reasoning",
                          inc=inc, latency_ms=res.latency_ms, tokens=res.tokens, model=res.model)

    async def _assess_reply(self, checkin_id: str, transcript: str, keyword: rules.ReplyClass) -> None:
        prompt = (
            "A ride-hailing safety agent asked a rider whether they are okay after a route deviation or hard "
            "brake. Classify the rider's reply. distress=true if they may be in danger or need help.\n"
            f"Reply: {json.dumps(transcript[:500])}"
        )
        llm = await self.llm()
        res = await llm.complete_json(prompt, ReplyAssessment, CHECKIN_AGENT, lambda: {
            "distress": keyword.label == rules.DISTRESS, "sentiment": "unknown", "confidence": keyword.confidence})
        c = self.store.checkins[checkin_id]
        inc = self.store.incidents[c["incident_id"]]
        source = "llm" if not res.used_fallback else f"keyword fallback ({res.fallback_reason})"
        await self.store.update_checkin(checkin_id, llm_classification={**res.data, "source": source})
        await self._step(inc, "llm_classification", AGENT,
                         f"LLM reply assessment: distress={res.data['distress']}, sentiment {res.data['sentiment']}, "
                         f"confidence {float(res.data['confidence']):.2f} ({source})",
                         used_fallback=res.used_fallback, fallback_reason=res.fallback_reason,
                         latency_ms=res.latency_ms, confidence=float(res.data["confidence"]), source=source,
                         assessment=res.data)
        if res.used_fallback:
            return
        up = rules.upgrade_action(inc["decision"], res.data)
        if up == rules.ESCALATE and inc["status"] not in ("escalated", "resolved"):
            await self._step(inc, "llm_upgrade", AGENT, "LLM detected distress the keywords missed: upgrading to escalate",
                             previous=inc["decision"])
            self.store.record_escalation(str(inc["driver_id"] or ""), self._now_s())
            await self._escalate(inc["id"], "LLM assessment of the rider's reply indicates distress")

    # ---- check-in ----

    async def _start_checkin(self, inc: Dict[str, Any]) -> Dict[str, Any]:
        question = rules.checkin_question(inc["triggers"])
        chk = await self.store.create_checkin(inc["id"], inc["ride_id"], question, self.checkin_timeout_s,
                                              driver_id=inc["driver_id"])
        await self.store.add_transcript(inc["id"], "agent", question, checkin_id=chk["id"])
        await self._step(inc, "checkin_started", AGENT,
                         f"calling the rider of ride {inc['ride_id']} (answer within {self.checkin_timeout_s:g} s)",
                         checkin_id=chk["id"], question=question, timeout_s=self.checkin_timeout_s)
        self._timers[chk["id"]] = self.spawn(self._checkin_timeout(chk["id"]), "checkin-timeout")
        self.spawn(self._push_call(inc, chk), "push-call")
        return chk

    def checkin_frame(self, inc: Dict[str, Any], chk: Dict[str, Any]) -> Dict[str, Any]:
        return public({
            "type": "safety_call", "checkin_id": chk["id"], "incident_id": inc["id"], "ride_id": inc["ride_id"],
            "driver_id": inc["driver_id"], "question": chk["question"], "timeout_s": chk["timeout_s"],
            "deadline_at": chk["deadline_at"], "severity": inc["severity"],
            "triggers": [t.get("type") for t in inc["triggers"]], "location": inc["location"],
        })

    async def _push_call(self, inc: Dict[str, Any], chk: Dict[str, Any]) -> None:
        frame = self.checkin_frame(inc, chk)
        via = {}
        for topic in (f"ride:{inc['ride_id']}", "ops"):
            via[topic] = await self.actions.publish(topic, frame)
        await self._step(inc, "call_pushed", AGENT, f"safety call pushed ({', '.join(f'{k} via {v}' for k, v in via.items())})",
                         event=False, channels=via, checkin_id=chk["id"])

    async def _checkin_timeout(self, checkin_id: str) -> None:
        c = self.store.checkins[checkin_id]
        await self.sleep(float(c["timeout_s"]))
        if c["status"] != "pending":
            return
        await self.store.update_checkin(checkin_id, status="timed_out")
        inc = self.store.incidents[c["incident_id"]]
        d = rules.decide_on_timeout(c["timeout_s"], inc["severity"])
        d = rules.apply_rate_limit(d, self.store.escalation_times(str(inc["driver_id"] or "")), self._now_s())
        await self.store.add_transcript(inc["id"], "system", f"No answer within {c['timeout_s']:g} s.",
                                        checkin_id=checkin_id)
        await self._step(inc, "checkin_timeout", AGENT, f"no reply within {c['timeout_s']:g} s: {d.action}",
                         d.reason, checkin_id=checkin_id, action=d.action)
        if d.suppressed:
            await self._suppress(inc, d)
            return
        self.store.record_escalation(str(inc["driver_id"] or ""), self._now_s())
        await self._escalate(inc["id"], d.reason)

    async def _suppress(self, inc: Dict[str, Any], d: Decision) -> None:
        self.store.record_suppressed({"incident_id": inc["id"], "ride_id": inc["ride_id"],
                                      "driver_id": inc["driver_id"], "severity": inc["severity"], "reason": d.reason})
        await self.store.update_incident(inc["id"], status="monitoring", decision=rules.MONITOR, suppressed=True)
        await self._step(inc, "escalation_suppressed", AGENT, "escalation suppressed by the rate limit (likely false positive)",
                         d.reason)

    async def handle_reply(self, checkin_id: str, transcript: str) -> Dict[str, Any]:
        """The rider answered: act on the keyword classification now, assess with the LLM later."""
        c = await self.store.get_checkin(checkin_id)
        if c is None or checkin_id not in self.store.checkins:
            raise CheckinNotFound(checkin_id)
        if c["status"] != "pending":
            raise CheckinClosed(c)
        text = (transcript or "").strip()[:1000]
        cls = rules.classify_reply(text)
        await self.store.update_checkin(checkin_id, status="answered", answered_at=self.clock(), transcript=text,
                                        classification={"label": cls.label, "matched": list(cls.matched),
                                                        "confidence": cls.confidence, "source": "keywords"})
        timer = self._timers.pop(checkin_id, None)
        if timer is not None:
            timer.cancel()
        inc = self.store.incidents[c["incident_id"]]
        await self.store.add_transcript(inc["id"], RIDER, text, checkin_id=checkin_id)
        await self._step(inc, "reply_received", RIDER, f"rider replied: {text!r}", checkin_id=checkin_id, text=text)
        d = rules.decide_after_reply(cls, inc["severity"])
        d = rules.apply_rate_limit(d, self.store.escalation_times(str(inc["driver_id"] or "")), self._now_s(),
                                   rider_confirmed=cls.label == rules.DISTRESS)
        await self._step(inc, "reply_classified", AGENT, f"reply classified {cls.label} by keywords: {d.action}",
                         d.reason, confidence=cls.confidence, label=cls.label, matched=list(cls.matched),
                         action=d.action)
        if d.action == rules.ESCALATE:
            self.store.record_escalation(str(inc["driver_id"] or ""), self._now_s())
            await self.store.update_incident(inc["id"], decision=rules.ESCALATE, reason=d.reason)
            self.spawn(self._escalate(inc["id"], d.reason), "escalate")
            status = "escalated"
        elif d.suppressed:
            await self._suppress(inc, d)
            status = "monitoring"
        else:
            await self.store.update_incident(inc["id"], status="monitoring", decision=rules.MONITOR, reason=d.reason)
            await self._step(inc, "monitoring", AGENT, f"rider is safe: monitoring ride {inc['ride_id']}", d.reason)
            self.spawn(self._push_update(inc, "monitoring", "Thanks - glad you're safe. We'll keep an eye on your trip.",
                                         checkin_id), "push-update")
            status = "monitoring"
        self.spawn(self._assess_reply(checkin_id, text, cls), "assess-reply")
        return {"checkin_id": checkin_id, "incident_id": inc["id"], "action": d.action, "incident_status": status,
                "classification": {"label": cls.label, "matched": list(cls.matched), "confidence": cls.confidence}}

    async def _push_update(self, inc: Dict[str, Any], status: str, message: str, checkin_id: Optional[str] = None,
                           **extra: Any) -> None:
        frame = public({"type": "safety_update", "incident_id": inc["id"], "ride_id": inc["ride_id"],
                        "checkin_id": checkin_id, "status": status, "message": message, **extra})
        via = {t: await self.actions.publish(t, frame) for t in (f"ride:{inc['ride_id']}", "ops")}
        await self.store.add_step(inc["id"], "rider_informed", AGENT, summary=f"rider informed: {status}",
                                  channels=via, message=message)

    # ---- escalation ----

    async def _escalate(self, incident_id: str, reason: str) -> None:
        async with self._lock(incident_id):
            inc = self.store.incidents[incident_id]
            if inc["actions"].get("escalated_at") or inc["status"] == "resolved":
                return
            await self.store.set_action(incident_id, "escalated_at", {"at": self.clock(), "reason": reason})
            await self.store.update_incident(incident_id, status="escalated", decision=rules.ESCALATE, reason=reason)
            await self._step(inc, "escalated", AGENT, f"escalating incident {incident_id}: {reason}", reason,
                             confidence=inc["severity"])
            ride_id, driver_id = str(inc["ride_id"] or ""), str(inc["driver_id"] or "")
            ok = 0

            payload = public({
                "incident_id": incident_id, "severity": inc["severity"], "reason": reason,
                "location": inc["location"], "triggers": inc["triggers"],
                "trip": {"ride_id": ride_id, "rider_id": inc.get("rider_id"), "pickup": inc.get("pickup"),
                         "dropoff": inc.get("dropoff")},
                "driver": {"driver_id": driver_id, "prior_incidents": len(await self.store.incidents_for_driver(driver_id)) - 1},
                "vehicle": {"id": driver_id, "tier": inc.get("tier"), "autonomous": inc.get("tier") == "av"},
            })
            try:
                res = await self.actions.emergency_dispatch(payload)
                await self.store.set_action(incident_id, "emergency_notified", {**res, "status": "ok",
                                                                                "service_status": res.get("status")})
                await self._step(inc, "emergency_dispatched", AGENT,
                                 f"emergency services dispatched (ref {res.get('reference')}, ETA {res.get('eta_min')} min, simulated)",
                                 reference=res.get("reference"), payload=payload)
                ok += 1
            except ActionError as err:
                await self._failed(inc, "emergency_notified", "emergency_dispatch_failed", err)

            url = None
            try:
                res = await self.actions.create_track_link(driver_id, ride_id, incident_id)
                url = res.get("url") or res.get("path")
                await self.store.set_action(incident_id, "location_shared", {"status": "ok", "url": url,
                                                                             "token": res.get("token")})
                await self._step(inc, "location_shared", AGENT, f"live location shared: {url}", url=url)
                ok += 1
            except ActionError as err:
                await self._failed(inc, "location_shared", "location_share_failed", err)

            try:
                res = await self.actions.suspend_driver(driver_id, incident_id, reason)
                result = res.get("result") or {}
                await self.store.set_action(incident_id, "driver_frozen", {**result, "status": "ok",
                                                                           "suspended_at": res.get("suspended_at")})
                await self._step(inc, "driver_suspended", AGENT,
                                 f"driver {driver_id} suspended; vehicle "
                                 f"{'stopped' if result.get('vehicle_stopped') else 'flagged'}", **result)
                if result.get("replacement_ride_id"):
                    await self.store.set_action(incident_id, "replacement_ride", {"ride_id": result["replacement_ride_id"]})
                    await self._step(inc, "replacement_dispatched", AGENT,
                                     f"replacement AV ride {result['replacement_ride_id']} dispatched for the rider",
                                     ride_id=result["replacement_ride_id"])
                ok += 1
            except ActionError as err:
                await self._failed(inc, "driver_frozen", "driver_suspend_failed", err)

            contact = emergency_contact(str(inc.get("rider_id") or ""))
            message = (f"HexaGo Safety: a possible incident on a ride with {inc.get('rider_id') or 'your contact'}. "
                       f"Emergency services are informed. Live location: {url or 'unavailable'}")
            try:
                res = await self.actions.notify_contact({"incident_id": incident_id, "rider_id": inc.get("rider_id"),
                                                         "contact": contact, "message": message})
                await self.store.set_action(incident_id, "contact_notified", {**res, "status": "ok", "contact": contact,
                                                                              "service_status": res.get("status")})
                await self._step(inc, "contact_notified", AGENT, f"emergency contact notified ({contact['phone']}, simulated)",
                                 contact=contact, reference=res.get("reference"))
                ok += 1
            except ActionError as err:
                await self._failed(inc, "contact_notified", "contact_notify_failed", err)

            await self._push_update(inc, "escalated", "Help is on the way. Emergency services have been notified and "
                                                      "your live location is being shared.", track_url=url)
            await self._event(f"incident {incident_id} escalated: {ok}/4 actions completed",
                              f"{reason}. Driver {driver_id} stays suspended until the Ops Supervisor confirms a resolution.",
                              kind="safety_escalation", inc=inc, confidence=inc["severity"], actions_ok=ok)

    async def _failed(self, inc: Dict[str, Any], action: str, step: str, err: Exception) -> None:
        await self.store.set_action(inc["id"], action, {"status": "failed", "error": str(err)[:300]})
        await self._step(inc, step, AGENT, f"{action.replace('_', ' ')} failed: {err}", used_fallback=True,
                         fallback_reason="error", error=str(err)[:300])

    # ---- resolution (Ops Supervisor only) ----

    async def resolve(self, incident_id: str, requested_by: str, note: str) -> Dict[str, Any]:
        inc = await self.store.get_incident(incident_id)
        if inc is None or incident_id not in self.store.incidents:
            raise IncidentNotFound(incident_id)
        pending = sum(1 for c in self.store.checkins_for(incident_id) if c["status"] == "pending")
        conf = await self._supervisor(public(inc), requested_by=requested_by, note=note, pending_checkins=pending)
        await self.store.add_step(incident_id, "supervisor_review", SUPERVISOR,
                                  summary="resolution confirmed" if conf.valid else "resolution rejected",
                                  approved=conf.valid, violations=list(conf.violations), requested_by=requested_by)
        if not conf.valid:
            raise ResolutionRejected(conf)
        frozen = inc["actions"].get("driver_frozen") or {}
        if frozen.get("status") == "ok":
            res = await self.actions.unsuspend_driver(str(inc["driver_id"]), incident_id, conf)
            await self.store.set_action(incident_id, "driver_unfrozen", {**res, "status": "ok", "by": conf.agent})
            await self._step(inc, "driver_unfrozen", SUPERVISOR, f"driver {inc['driver_id']} returned to service",
                             confirmed_by=conf.agent)
        await self.store.close_incident(incident_id, conf, note.strip(), f"{requested_by} (confirmed by {conf.agent})")
        await self._step(inc, "resolved", SUPERVISOR, f"incident {incident_id} resolved: {note.strip()}",
                         requested_by=requested_by)
        return self.store.snapshot(inc)

    # ---- views ----

    def summary(self, inc: Dict[str, Any]) -> Dict[str, Any]:
        actions = inc.get("actions") or {}
        pending = [c["id"] for c in self.store.checkins_for(inc["id"]) if c["status"] == "pending"]
        return public({
            "id": inc["id"], "ride_id": inc.get("ride_id"), "driver_id": inc.get("driver_id"),
            "rider_id": inc.get("rider_id"), "tier": inc.get("tier"), "severity": inc.get("severity"),
            "status": inc.get("status"), "decision": inc.get("decision"), "reason": inc.get("reason"),
            "triggers": inc.get("triggers") or [], "location": inc.get("location"),
            "created_at": inc.get("created_at"), "updated_at": inc.get("updated_at"),
            "resolved_at": inc.get("resolved_at"), "pending_checkin_id": pending[0] if pending else None,
            "flags": {k: (actions.get(k) or {}).get("status") == "ok"
                      for k in ("driver_frozen", "emergency_notified", "location_shared", "contact_notified")},
        })

    async def detail(self, incident_id: str) -> Dict[str, Any]:
        inc = await self.store.get_incident(incident_id)
        if inc is None:
            raise IncidentNotFound(incident_id)
        out = self.store.snapshot(inc)
        out["checkins"] = [public(c) for c in self.store.checkins_for(incident_id)]
        since = inc["created_at"] - dt.timedelta(minutes=3)
        try:
            path = await self.actions.vehicle_path(str(inc.get("driver_id") or ""), since=since)
            out["vehicle_path"], out["path_source"] = path, "telemetry"
        except ActionError as err:
            log.info("vehicle path for %s unavailable: %s", incident_id, err)
            path = []
        if not path:
            out["vehicle_path"] = [{"lat": p.get("lat"), "lng": p.get("lng"), "speed_kmh": p.get("speed_kmh"),
                                    "recorded_at": p.get("at")} for p in inc.get("trail") or []]
            out["path_source"] = "signal_trail"
        out["deviation_m"] = rules.deviation_threshold_m()
        out["summary"] = self.summary(inc)
        return out

    def pending_checkins(self) -> List[Dict[str, Any]]:
        out = []
        now = self.clock()
        for c in self.store.pending_checkins():
            inc = self.store.incidents.get(c["incident_id"]) or {}
            out.append({**(self.checkin_frame(inc, c) if inc else public(c)),
                        "remaining_s": max(0.0, round((c["deadline_at"] - now).total_seconds(), 1)),
                        "status": c["status"]})
        return out

    def checkin_view(self, c: Dict[str, Any]) -> Dict[str, Any]:
        inc = self.store.incidents.get(c["incident_id"]) or {}
        base = self.checkin_frame(inc, c) if inc else public(c)
        return {**base, "status": c["status"], "transcript": c.get("transcript"),
                "classification": c.get("classification"), "llm_classification": c.get("llm_classification"),
                "remaining_s": max(0.0, round((c["deadline_at"] - self.clock()).total_seconds(), 1)),
                "incident_status": inc.get("status")}


# ---- process-wide sentinel ----
_sentinel: Optional[SafetySentinel] = None


async def get_sentinel() -> SafetySentinel:
    global _sentinel
    if _sentinel is None:
        from src.agents_py.core import infra

        _sentinel = SafetySentinel(SafetyStore(pool=await infra.get_pool()), SafetyActions())
    elif _sentinel.store.pool is None:
        from src.agents_py.core import infra

        _sentinel.store.pool = await infra.get_pool()
    return _sentinel


def set_sentinel(s: Optional[SafetySentinel]) -> None:
    global _sentinel
    _sentinel = s
