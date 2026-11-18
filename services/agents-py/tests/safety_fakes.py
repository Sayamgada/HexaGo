"""Doubles for the Safety Sentinel: dispatch-go actions, an LLM, a manual clock and sleep."""
import asyncio
import datetime as dt
from typing import Any, Dict, List, Optional

from src.agents_py.agents.safety.actions import ActionError
from src.agents_py.agents.safety.sentinel import SafetySentinel
from src.agents_py.agents.safety.store import SafetyStore
from src.agents_py.core.llm import LLMResult


class FakeActions:
    def __init__(self, fail: Optional[set] = None, tier: str = "sedan"):
        self.fail = set(fail or ())
        self.calls: List[tuple] = []
        self.published: List[tuple] = []
        self.ride = {"id": "ride1", "rider_id": "rider-1", "tier": tier, "status": "in_progress",
                     "pickup": {"lat": 12.97, "lng": 77.59}, "dropoff": {"lat": 12.99, "lng": 77.61}}
        self.path: List[Dict[str, Any]] = [{"lat": 12.97, "lng": 77.59, "speed_kmh": 30, "recorded_at": "t"}]
        self.ride_delay = 0.0

    def _maybe_fail(self, name: str) -> None:
        self.calls.append((name,))
        if name in self.fail:
            raise ActionError(f"{name} failed")

    async def get_ride(self, ride_id: str):
        if self.ride_delay:
            await asyncio.sleep(self.ride_delay)
        self._maybe_fail("get_ride")
        return dict(self.ride, id=ride_id)

    async def suspend_driver(self, driver_id, incident_id, reason):
        self._maybe_fail("suspend")
        result = {"driver_id": driver_id, "tier": self.ride["tier"], "vehicle_stopped": True,
                  "interrupted_ride_id": "ride1"}
        if self.ride["tier"] == "av":
            result["replacement_ride_id"] = "ride2"
        return {"driver_id": driver_id, "incident_id": incident_id, "result": result, "suspended_at": "now"}

    async def unsuspend_driver(self, driver_id, incident_id, confirmation):
        if confirmation is None or not confirmation.valid or confirmation.incident_id != incident_id:
            raise PermissionError("needs supervisor")
        self._maybe_fail("unsuspend")
        return {"driver_id": driver_id, "suspended": False}

    async def create_track_link(self, driver_id, ride_id, incident_id):
        self._maybe_fail("track")
        return {"token": "tok", "url": "https://gw/track/tok", "path": "/track/tok"}

    async def emergency_dispatch(self, payload):
        self._maybe_fail("emergency")
        self.calls.append(("emergency_payload", payload))
        return {"reference": "EMS-1", "status": "dispatched", "eta_min": 7}

    async def notify_contact(self, payload):
        self._maybe_fail("contact")
        return {"reference": "SMS-1", "status": "sent"}

    async def vehicle_path(self, driver_id, since=None, limit=900):
        self._maybe_fail("path")
        return list(self.path)

    async def publish(self, topic, data):
        self.published.append((topic, data))
        return "gateway"

    def names(self) -> List[str]:
        return [c[0] for c in self.calls]


class FakeLLM:
    """complete_json stand-in. `gate` (an Event) makes it hang until set; `fallback` makes it fall back."""

    def __init__(self, reasoning: str = "LLM says: risky swerve.", assessment: Optional[Dict[str, Any]] = None,
                 fallback: Optional[str] = None):
        self.reasoning = reasoning
        self.assessment = assessment or {"distress": False, "sentiment": "calm", "confidence": 0.9}
        self.fallback = fallback
        self.gate: Optional[asyncio.Event] = None
        self.calls: List[str] = []

    async def complete_json(self, prompt, schema, agent_name, fallback):
        self.calls.append(agent_name)
        if self.gate is not None:
            await self.gate.wait()
        if self.fallback:
            return LLMResult(fallback(), True, self.fallback, model="deterministic-fallback")
        data = {"reasoning": self.reasoning, "risk": "high"} if "reasoning" in schema.model_fields else dict(self.assessment)
        return LLMResult(schema.model_validate(data).model_dump(), False, model="gemini/test", tokens=42, latency_ms=5)


class ManualSleep:
    """asyncio.sleep stand-in: waits until `fire()` (check-in timeouts on demand)."""

    def __init__(self):
        self.waits: List[float] = []
        self.event = asyncio.Event()

    async def __call__(self, s: float) -> None:
        self.waits.append(s)
        await self.event.wait()

    def fire(self) -> None:
        self.event.set()


class Clock:
    def __init__(self):
        self.t = dt.datetime(2026, 11, 18, 6, 30, tzinfo=dt.timezone.utc)

    def __call__(self) -> dt.datetime:
        return self.t

    def advance(self, s: float) -> None:
        self.t += dt.timedelta(seconds=s)


class Recorder:
    def __init__(self):
        self.events: List[Dict[str, Any]] = []

    async def __call__(self, agent, summary, decision, reasoning, confidence, latency_ms, used_fallback,
                       fallback_reason=None, metadata=None):
        self.events.append({"agent": agent, "decision": decision, "reasoning": reasoning, "kind": (metadata or {}).get("kind"),
                            "used_fallback": used_fallback, "fallback_reason": fallback_reason, "metadata": metadata})

    def kinds(self) -> List[str]:
        return [e["kind"] for e in self.events]


def make_sentinel(actions=None, llm=None, **kw):
    clock = kw.pop("clock", None) or Clock()
    sleep = kw.pop("sleep", None) or ManualSleep()
    rec = kw.pop("record", None) or Recorder()
    s = SafetySentinel(SafetyStore(clock=clock, pool=kw.pop("pool", None)), actions or FakeActions(),
                       llm=llm or FakeLLM(), record=rec, demo=kw.pop("demo", True), clock=clock, sleep=sleep, **kw)
    return s, rec, clock, sleep


def signal(severity: float = 0.46, ride: str = "ride1", driver: str = "d_001", **extra) -> Dict[str, Any]:
    sig = {"type": "safety_signal", "signal_id": "sig_1", "ride_id": ride, "driver_id": driver, "rider_id": "rider-1",
           "tier": "sedan", "severity": severity,
           "triggers": [{"type": "route_deviation", "value": 420, "unit": "m", "duration_s": 10, "severity": severity}],
           "location": {"lat": 12.98, "lng": 77.6}, "planned_route": [[12.97, 77.59], [12.99, 77.61]],
           "trail": [{"lat": 12.975, "lng": 77.595, "speed_kmh": 40, "at": "2026-11-18T06:29:50Z"}]}
    sig.update(extra)
    return sig
