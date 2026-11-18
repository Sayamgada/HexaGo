"""Booking test data: 10 golden utterances (expected legs + a mocked LLM answer) and fakes."""
import datetime as dt
from typing import Any, Dict, List, Optional

from src.agents_py.agents.booking.itinerary import IST, TripEstimate
from src.agents_py.agents.booking.places import GAZETTEER, Place
from src.agents_py.agents.booking.planner import BookingAgent
from src.agents_py.agents.pricing.quote import CellLoad

NOW = dt.datetime(2026, 11, 18, 8, 0, tzinfo=IST)  # Wednesday 8:00 AM in Bengaluru

GOLDEN_SENTENCE = ("Pick up my kid from school at 3:15 PM, drop them at soccer practice, and then book a high-tier "
                   "sedan for me from my office to the airport to arrive by 6:00 PM.")


def leg(passenger, origin, destination, pickup_at=None, arrive_by=None, tier=None, day_offset=0, label=None):
    return {"passenger": passenger, "passenger_label": label, "origin": origin, "destination": destination,
            "pickup_at": pickup_at, "arrive_by": arrive_by, "tier": tier, "day_offset": day_offset, "asap": False}


# (text, expected legs as (passenger, origin, destination, requested pickup, arrive_by, tier, day_offset),
#  mocked LLM JSON). Saved places are compared by key, geocoded ones by label; "current" = no origin given.
GOLDEN: List[tuple] = [
    (GOLDEN_SENTENCE,
     [("kid", "kid_school", "soccer", "15:15", None, "mini", 0),
      ("me", "office", "airport", None, "18:00", "premium", 0)],
     {"legs": [leg("kid", "kid's school", "Soccer practice", pickup_at="15:15", label="my kid"),
               leg("me", "my office", "the airport", arrive_by="18:00", tier="premium")]}),
    ("Book a cab from home to the office at 9:30 AM",
     [("me", "home", "office", "09:30", None, "mini", 0)],
     {"legs": [leg("me", "home", "office", pickup_at="09:30")]}),
    ("I need to reach the airport by 6 pm in an XL",
     [("me", "current", "airport", None, "18:00", "xl", 0)],
     {"legs": [leg("me", None, "airport", arrive_by="18:00", tier="XL")]}),
    ("Get an auto from office to home at 7:15 pm",
     [("me", "office", "home", "19:15", None, "auto", 0)],
     {"legs": [leg("me", "office", "home", pickup_at="19:15", tier="auto")]}),
    ("Send a premium car for my kid from home to soccer practice at 4 pm",
     [("kid", "home", "soccer", "16:00", None, "premium", 0)],
     {"legs": [leg("kid", "home", "soccer practice", pickup_at="16:00", tier="premium", label="my kid")]}),
    ("Pool ride from home to MG Road at 8:45am",
     [("me", "home", "MG Road", "08:45", None, "pool", 0)],
     {"legs": [leg("me", "home", "MG Road", pickup_at="08:45", tier="pool")]}),
    ("Take me to the airport tomorrow at 5:30 AM in a self-driving car",
     [("me", "current", "airport", "05:30", None, "av", 1)],
     {"legs": [leg("me", None, "Kempegowda airport", pickup_at="05:30", tier="av", day_offset=1)]}),
    ("From office to home at 6:30 pm, then from home to the airport to arrive by 10 pm",
     [("me", "office", "home", "18:30", None, "mini", 0),
      ("me", "home", "airport", None, "22:00", "mini", 0)],
     {"legs": [leg("me", "office", "home", pickup_at="18:30"),
               leg("me", "home", "airport", arrive_by="22:00")]}),
    ("Pick up my daughter from school at 3:30 PM and drop her at home",
     [("kid", "kid_school", "home", "15:30", None, "mini", 0)],
     {"legs": [leg("daughter", "school", "home", pickup_at="15:30", label="my daughter")]}),
    ("Sedan for me from Koramangala to Indiranagar at 11 am",
     [("me", "Koramangala", "Indiranagar", "11:00", None, "sedan", 0)],
     {"legs": [leg("me", "Koramangala", "Indiranagar", pickup_at="11:00", tier="sedan")]}),
]


def parsed_summary(parsed) -> List[tuple]:
    """Fallback-parser output in the shape of GOLDEN (before place resolution)."""
    return [(lg.passenger, lg.origin, lg.destination, lg.pickup_at, lg.arrive_by, lg.tier, lg.day_offset)
            for lg in parsed.legs]


def plan_summary(plan: Dict[str, Any]) -> List[tuple]:
    def place(p):
        return p["key"] if p["source"] in ("saved", "current") else p["label"]

    return [(lg["passenger"], place(lg["origin"]), place(lg["destination"]), lg["requested_pickup_time"],
             lg["arrive_by"][11:16] if lg["arrive_by"] else None, lg["tier"],
             (dt.date.fromisoformat(lg["pickup_at"][:10]) - NOW.date()).days)
            for lg in sorted(plan["legs"], key=lambda x: x["seq"])]


class FakeGeocoder:
    """Answers from the built-in gazetteer; records queries."""

    def __init__(self, known: Optional[Dict[str, tuple]] = None):
        self.known = known if known is not None else GAZETTEER
        self.queries: List[str] = []

    async def geocode(self, text):
        self.queries.append(text)
        hit = self.known.get(text.lower().strip())
        if hit is None:
            return None
        label, lat, lng = hit
        return Place(label, lat, lng, "geocoded", query=text)


class FakeGateway:
    def __init__(self, eta_fails=True, load=None, schedule_error=None):
        self.eta_fails = eta_fails
        self.load = load or CellLoad(cell="89618", supply=10, demand=5, source="dispatch-go")
        self.schedule_error = schedule_error
        self.scheduled: List[dict] = []
        self.eta_calls = 0

    async def eta(self, a, b):
        self.eta_calls += 1
        if self.eta_fails:
            raise ConnectionError("dispatch-go down")
        return TripEstimate(12.0, 30.0, "dispatch-go")

    async def cell_load(self, lat, lng):
        return self.load

    async def schedule(self, payload):
        if self.schedule_error:
            raise self.schedule_error
        self.scheduled.append(payload)
        return {"plan_id": payload["plan_id"], "demo_mode": True,
                "legs": [{"id": f"sch_{i}", "seq": lg["seq"], "status": "scheduled"}
                         for i, lg in enumerate(payload["legs"], start=1)]}


class Recorder:
    def __init__(self):
        self.events: List[dict] = []

    async def __call__(self, agent, input_summary, decision, reasoning, confidence, latency_ms, used_fallback,
                       fallback_reason=None, metadata=None):
        self.events.append({"agent": agent, "input": input_summary, "decision": decision, "reasoning": reasoning,
                            "confidence": confidence, "used_fallback": used_fallback,
                            "fallback_reason": fallback_reason, "metadata": metadata or {}})


def make_agent(llm=None, gateway=None, geocoder=None, now=NOW, **kw):
    rec = Recorder()
    agent = BookingAgent(llm=llm, gateway=gateway or FakeGateway(), geocoder=geocoder or FakeGeocoder(),
                         record=rec, clock=lambda: now, demo=True, **kw)
    return agent, rec
