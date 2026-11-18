"""Multimodal Booking Intent agent: one sentence -> a priced, timed multi-leg plan.

LangGraph flow (``BookingAgent.graph``)::

    parse ──question?──> present
      └─> resolve ──question?──> present
            └─> itinerary -> feasibility -> price -> present -> END

* parse        LLM (``core.llm.complete_json``) extracts a ``ParsedRequest``; the
               rule-based ``fallback_parser`` runs when the LLM is disabled, out of
               budget, rate-limited, times out or answers malformed JSON (or when
               ``force_fallback`` is set in DEMO_MODE).
* resolve      saved places of the rider persona first, then Nominatim.
* itinerary    trip estimates (dispatch-go, haversine fallback) and pickup times,
               worked backwards from arrive-by.
* feasibility  timing warnings, tier checks, city bounds.
* price        ``pricing.quote`` per leg with the pickup cell's surge.
* present      a plan for confirmation, or a clarifying question with quick answers.

``confirm(plan_id)`` hands the legs to dispatch-go's scheduler (POST /v1/scheduled).
Every parse, question and confirm is written to ``agent_events``.
"""
from __future__ import annotations

import asyncio
import collections
import datetime as dt
import logging
import secrets
import time
from typing import Any, Awaitable, Callable, Dict, List, Optional, TypedDict

from langgraph.graph import END, START, StateGraph

from src.agents_py.agents.booking import fallback_parser
from src.agents_py.agents.booking.gateway import DispatchGateway, GatewayError
from src.agents_py.agents.booking.itinerary import (
    IST, LegTiming, TimedLeg, estimate, fmt_time, plan_times, short,
)
from src.agents_py.agents.booking.models import ParsedRequest
from src.agents_py.agents.booking.places import (
    Geocoder, Persona, Place, get_persona, haversine_km, in_city, normalize, resolve_place,
)
from src.agents_py.agents.pricing.quote import RATES, CellLoad, RatioSurge, SurgeModel, quote
from src.agents_py.config import settings
from src.agents_py.core import events
from src.agents_py.core.llm import get_llm

log = logging.getLogger("hexago.booking")

AGENT = "Booking Intent"
LLM_AGENT = "booking_intent"
RULE_BASED = "rule-based"
PLAN_TTL_S = 3600
MAX_KEPT = 500


class PlanNotFound(KeyError):
    pass


class QuestionNotFound(KeyError):
    pass


class BookingState(TypedDict, total=False):
    rider_id: str
    text: str
    force_fallback: bool
    started: float
    now: dt.datetime
    persona: Persona
    parsed: Dict[str, Any]
    answered_by: Dict[str, Any]
    places: List[Dict[str, Place]]
    timed: List[TimedLeg]
    warnings: List[str]
    tiers: List[str]
    prices: List[Dict[str, Any]]
    question: Optional[Dict[str, Any]]
    result: Dict[str, Any]


def _ms(start: float) -> float:
    return round((time.perf_counter() - start) * 1000, 1)


def _new_id(prefix: str) -> str:
    return f"{prefix}_{secrets.token_hex(6)}"


def model_label(model: str) -> str:
    return model.split("/", 1)[-1] if model and model != "deterministic-fallback" else RULE_BASED


def build_prompt(text: str, persona: Persona, now: dt.datetime) -> str:
    places = "; ".join(f'"{p.aliases[0] if p.aliases else p.key}" = {p.label}' for p in persona.places)
    kids = ", ".join(persona.kids) or "none"
    return (
        "You are HexaGo's Booking Intent agent for ride-hailing in Bengaluru, India.\n"
        f"Local time now: {now.astimezone(IST):%A %Y-%m-%d %H:%M} IST.\n"
        f"Rider: {persona.name}. Children: {kids}. Saved places: {places}.\n"
        "Split the request into ride legs in the order they happen. For each leg give: passenger "
        '("me" for the rider, "kid" for their child, or a relation such as "mom"), passenger_label (their words), '
        "origin and destination as the rider named them (use the saved place names above when they match; "
        "origin null = the rider's current location), pickup_at or arrive_by as 24h HH:MM, day_offset "
        "(1 = tomorrow), tier (auto | mini | sedan | premium | xl | av | pool; a high-tier/luxury sedan is "
        "premium, self-driving is av; null if not said) and asap (true for now / asap).\n"
        '"Pick up X from A at T, drop them at B" is ONE leg A -> B for X with pickup_at T. '
        "Do not invent legs, places or times the rider did not say. Put anything you cannot determine in "
        "ambiguities.\n"
        f'Request: """{text}"""'
    )


class BookingAgent:
    def __init__(self, *, llm: Any = None, geocoder: Optional[Geocoder] = None,
                 gateway: Optional[DispatchGateway] = None,
                 record: Optional[Callable[..., Awaitable[Any]]] = None,
                 clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(IST),
                 surge: Optional[SurgeModel] = None, demo: Optional[bool] = None):
        self._llm = llm
        self.geocoder = geocoder if geocoder is not None else Geocoder()
        self.gateway = gateway or DispatchGateway()
        self.record = record or events.record_event
        self.clock = clock
        self.surge = surge or RatioSurge()
        self.demo = settings.DEMO_MODE if demo is None else demo
        self.plans: "collections.OrderedDict[str, Dict[str, Any]]" = collections.OrderedDict()
        self.drafts: "collections.OrderedDict[str, Dict[str, Any]]" = collections.OrderedDict()
        self.confirmed: Dict[str, Dict[str, Any]] = {}
        self.graph = self._build()

    # ---- graph ----
    def _build(self):
        g = StateGraph(BookingState)
        g.add_node("parse", self._parse)
        g.add_node("resolve", self._resolve)
        g.add_node("itinerary", self._itinerary)
        g.add_node("feasibility", self._feasibility)
        g.add_node("price", self._price)
        g.add_node("present", self._present)
        g.add_edge(START, "parse")
        g.add_conditional_edges("parse", lambda s: "present" if s.get("question") else "resolve")
        g.add_conditional_edges("resolve", lambda s: "present" if s.get("question") else "itinerary")
        g.add_edge("itinerary", "feasibility")
        g.add_edge("feasibility", "price")
        g.add_edge("price", "present")
        g.add_edge("present", END)
        return g.compile()

    async def _llm_client(self):
        return self._llm if self._llm is not None else await get_llm()

    async def _parse(self, state: BookingState) -> Dict[str, Any]:
        if state.get("parsed") is not None:  # answering a clarifying question: the draft is reused
            return {"question": self._missing(state["parsed"], state["persona"], state["now"])}
        text = state["text"]
        persona = state["persona"]
        if state.get("force_fallback"):
            t0 = time.perf_counter()
            parsed = fallback_parser.parse(text).model_dump()
            meta = {"model": RULE_BASED, "label": RULE_BASED, "used_fallback": True, "fallback_reason": "forced",
                    "cached": False, "tokens": 0, "latency_ms": _ms(t0)}
        else:
            llm = await self._llm_client()
            res = await llm.complete_json(build_prompt(text, persona, state["now"]), ParsedRequest, LLM_AGENT,
                                          lambda: fallback_parser.parse(text))
            parsed = res.data
            meta = {"model": res.model if not res.used_fallback else RULE_BASED, "label": model_label(res.model)
                    if not res.used_fallback else RULE_BASED, "used_fallback": res.used_fallback,
                    "fallback_reason": res.fallback_reason, "cached": res.cached, "tokens": res.tokens,
                    "latency_ms": res.latency_ms}
        return {"parsed": parsed, "answered_by": meta,
                "question": self._missing(parsed, persona, state["now"])}

    def _missing(self, parsed: Dict[str, Any], persona: Persona, now: dt.datetime) -> Optional[Dict[str, Any]]:
        """A clarifying question if a required field is missing."""
        legs = parsed.get("legs") or []
        if not legs:
            return {"field": "text", "question": "Where would you like to go? You can describe your whole plan.",
                    "options": ["Home", "Office", "Airport"]}
        multi = len(legs) > 1
        for i, leg in enumerate(legs):
            who = self._who(leg, persona)
            if not leg.get("destination"):
                return {"field": f"destination:{i}", "question": f"Where should {who} be dropped off?",
                        "options": [short(p.label) for p in persona.places if p.key != "home"][:4] + ["Home"]}
            timed = leg.get("pickup_at") or leg.get("arrive_by") or leg.get("asap")
            needs_time = leg.get("passenger", "me") != "me" or (multi and i == 0)
            if not timed and needs_time and not self._chained(legs, i):
                where = f" from {leg['origin']}" if leg.get("origin") else ""
                soon = [fmt_time(now + dt.timedelta(minutes=m)) for m in (30, 60)]
                return {"field": f"time:{i}", "question": f"What time should we pick up {who}{where}?",
                        "options": ["Now", *soon]}
        return None

    @staticmethod
    def _chained(legs: List[Dict[str, Any]], i: int) -> bool:
        p = legs[i].get("passenger", "me")
        return any(lg.get("passenger", "me") == p for lg in legs[:i])

    @staticmethod
    def _who(leg: Dict[str, Any], persona: Persona) -> str:
        p = leg.get("passenger") or "me"
        if p == "me":
            return "you"
        if p == "kid" and len(persona.kids) == 1:
            return persona.kids[0]
        return leg.get("passenger_label") or f"your {p}"

    async def _resolve(self, state: BookingState) -> Dict[str, Any]:
        persona = state["persona"]
        hint = next((k for k in persona.kids if k.lower() in (state.get("text") or "").lower()), None)
        out: List[Dict[str, Place]] = []
        for i, leg in enumerate(state["parsed"]["legs"]):
            ends: Dict[str, Place] = {}
            for end in ("origin", "destination"):
                text = leg.get(end)
                if text and text.startswith("@"):  # chosen from a question's quick answers
                    p = next((sp for sp in persona.places if sp.key == text[1:]), None)
                    if p is not None:
                        ends[end] = Place(p.label, p.lat, p.lng, "saved", p.key, text)
                        continue
                res = await resolve_place(persona, text, self.geocoder, kid_hint=hint)
                if res.ambiguous:
                    labels = [short(p.label) if " - " in p.label else p.label for p in res.options]
                    names = [p.label.split(" - ")[0] for p in res.options]
                    return {"question": {"field": f"place:{i}:{end}", "keys": [p.key for p in res.options],
                                         "question": f"Which {normalize(text)} - {' or '.join(names)}?",
                                         "options": labels}}
                if res.place is None:
                    return {"question": {"field": f"placetext:{i}:{end}",
                                         "question": f'I couldn\'t find "{text}" in Bengaluru. Which area or '
                                                     f"landmark is it near?",
                                         "options": ["MG Road", "Koramangala", "Indiranagar"]}}
                ends[end] = res.place
            out.append(ends)
        return {"places": out, "question": None}

    async def _itinerary(self, state: BookingState) -> Dict[str, Any]:
        legs = state["parsed"]["legs"]
        eta = self.gateway.eta if self.gateway is not None else None
        trips = await asyncio.gather(*[
            estimate(eta, (p["origin"].lat, p["origin"].lng), (p["destination"].lat, p["destination"].lng))
            for p in state["places"]])
        timings = [LegTiming(passenger=leg.get("passenger") or "me", origin_label=p["origin"].label,
                             destination_label=p["destination"].label, trip=trip, pickup_at=leg.get("pickup_at"),
                             arrive_by=leg.get("arrive_by"), day_offset=leg.get("day_offset") or 0,
                             asap=bool(leg.get("asap")))
                   for leg, p, trip in zip(legs, state["places"], trips)]
        timed, warnings = plan_times(timings, state["now"])
        return {"timed": timed, "warnings": warnings}

    async def _feasibility(self, state: BookingState) -> Dict[str, Any]:
        parsed, persona = state["parsed"], state["persona"]
        center = (settings.CITY_CENTER_LAT, settings.CITY_CENTER_LNG)
        warnings = list(state.get("warnings") or [])
        tiers: List[str] = []
        for t in state["timed"]:
            leg = parsed["legs"][t.index]
            places = state["places"][t.index]
            tier = (leg.get("tier") or parsed.get("tier") or persona.default_tier or "mini").lower()
            if tier not in RATES:
                t.warnings.append(f'Unknown car type "{tier}"; using {persona.default_tier}.')
                tier = persona.default_tier
            if tier == "pool" and leg.get("passenger") == "kid":
                t.warnings.append("Children can't ride Pool alone; booked a Mini instead.")
                tier = "mini"
            o, d = places["origin"], places["destination"]
            if haversine_km((o.lat, o.lng), (d.lat, d.lng)) < 0.2:
                t.warnings.append(f"Pickup and drop-off are the same place ({short(o.label)}).")
            for p in (o, d):
                if not in_city(p.lat, p.lng, center):
                    t.warnings.append(f"{short(p.label)} is outside the Bengaluru service area.")
            tiers.append(tier)
        return {"tiers": tiers, "warnings": warnings}

    async def _price(self, state: BookingState) -> Dict[str, Any]:
        async def one(t: TimedLeg, tier: str) -> Dict[str, Any]:
            o = state["places"][t.index]["origin"]
            load = await self.gateway.cell_load(o.lat, o.lng) if self.gateway is not None else CellLoad()
            return quote(tier, t.leg.trip.distance_km, t.leg.trip.duration_min, load, self.surge)

        prices = await asyncio.gather(*[one(t, tier) for t, tier in zip(state["timed"], state["tiers"])])
        return {"prices": list(prices)}

    async def _present(self, state: BookingState) -> Dict[str, Any]:
        q = state.get("question")
        if q:
            qid = _new_id("q")
            self._keep(self.drafts, qid, {"rider_id": state["rider_id"], "text": state["text"],
                                          "parsed": state.get("parsed"), "answered_by": state.get("answered_by"),
                                          **q})
            result = {"type": "question", "question_id": qid, "question": q["question"],
                      "options": q.get("options", []), "field": q["field"], "rider_id": state["rider_id"],
                      "answered_by": state.get("answered_by") or {}}
            return {"result": result}
        persona = state["persona"]
        legs = []
        for seq, (t, tier, price) in enumerate(zip(state["timed"], state["tiers"], state["prices"]), start=1):
            spec = state["parsed"]["legs"][t.index]
            places = state["places"][t.index]
            passenger = spec.get("passenger") or "me"
            legs.append({
                "seq": seq,
                "passenger": passenger,
                "passenger_name": persona.name.split()[0] if passenger == "me" else self._who(spec, persona),
                "passenger_label": spec.get("passenger_label"),
                "origin": places["origin"].public(),
                "destination": places["destination"].public(),
                "pickup_at": t.pickup.isoformat(),
                "pickup_time": fmt_time(t.pickup),
                "arrive_by": t.arrive_by.isoformat() if t.arrive_by else None,
                "arrive_by_time": fmt_time(t.arrive_by) if t.arrive_by else None,
                "eta_arrival": t.arrival.isoformat(),
                "eta_arrival_time": fmt_time(t.arrival),
                "requested_pickup_time": spec.get("pickup_at"),
                "distance_km": t.leg.trip.distance_km,
                "duration_min": t.leg.trip.duration_min,
                "eta_source": t.leg.trip.source,
                "tier": tier,
                "tier_label": RATES[tier].label,
                "price": price,
                "warnings": t.warnings,
            })
        plan_id = _new_id("pln")
        now = state["now"]
        result = {
            "type": "plan", "plan_id": plan_id, "rider_id": state["rider_id"], "text": state["text"],
            "rider_name": persona.name, "legs": legs, "warnings": state.get("warnings") or [],
            "total_fare": sum(lg["price"]["total"] for lg in legs), "currency": "INR",
            "answered_by": state.get("answered_by") or {}, "created_at": now.isoformat(),
            "expires_at": (now + dt.timedelta(seconds=PLAN_TTL_S)).isoformat(), "demo_mode": self.demo,
        }
        self._keep(self.plans, plan_id, result)
        return {"result": result}

    @staticmethod
    def _keep(store: "collections.OrderedDict[str, Any]", key: str, value: Any) -> None:
        store[key] = value
        while len(store) > MAX_KEPT:
            store.popitem(last=False)

    # ---- public API ----
    async def parse(self, rider_id: str, text: str, *, force_fallback: bool = False,
                    reply_to: Optional[str] = None) -> Dict[str, Any]:
        started = time.perf_counter()
        now = self.clock()
        persona = get_persona(rider_id)
        state: BookingState = {"rider_id": rider_id or persona.id, "text": text, "now": now, "persona": persona,
                               "force_fallback": force_fallback}
        if reply_to:
            draft = self.drafts.pop(reply_to, None)
            if draft is None:
                raise QuestionNotFound(reply_to)
            state.update(self._apply_answer(draft, text, now))
        out = await self.graph.ainvoke(state)
        result = out["result"]
        result["latency_ms"] = _ms(started)
        await self._audit(result, state.get("text") or text, reply_to)
        return result

    def _apply_answer(self, draft: Dict[str, Any], answer: str, now: dt.datetime) -> Dict[str, Any]:
        field = draft["field"]
        base = {"text": draft["text"], "answered_by": draft.get("answered_by")}
        if field == "text" or draft.get("parsed") is None:
            return {"text": f"{answer}"}
        parsed = {**draft["parsed"], "legs": [dict(lg) for lg in draft["parsed"]["legs"]]}
        kind, _, rest = field.partition(":")
        idx_s, _, end = rest.partition(":")
        leg = parsed["legs"][int(idx_s)]
        if kind == "destination":
            leg["destination"] = answer.strip()
        elif kind == "time":
            hhmm, asap = fallback_parser.parse_answer_time(answer, now.hour * 60 + now.minute)
            if hhmm is None and not asap:
                return {"text": f"{draft['text']} {answer}"}  # could not read it: parse the combination afresh
            leg["pickup_at"], leg["asap"] = hhmm, asap
        elif kind == "place":
            norm = normalize(answer)
            keys = draft.get("keys") or []
            chosen = next((k for k, label in zip(keys, draft.get("options") or [])
                           if normalize(label) == norm or norm in normalize(label) or normalize(label) in norm), None)
            chosen = chosen or next((k for k in keys if k.split("_")[-1] in norm), None)
            leg[end] = f"@{chosen}" if chosen else answer.strip()
        else:  # placetext
            leg[end] = answer.strip()
        return {**base, "parsed": parsed}

    async def _audit(self, result: Dict[str, Any], text: str, reply_to: Optional[str]) -> None:
        meta = result.get("answered_by") or {}
        if result["type"] == "plan":
            legs = result["legs"]
            decision = (f"planned {len(legs)} leg{'s' if len(legs) != 1 else ''} for {result['rider_id']} "
                        f"(total INR {result['total_fare']})")
            reasoning = "; ".join(f"{lg['passenger_name']}: {short(lg['origin']['label'])} -> "
                                  f"{short(lg['destination']['label'])} {lg['tier']} at {lg['pickup_time']}"
                                  for lg in legs)
            if result["warnings"]:
                reasoning += " | warnings: " + " ".join(result["warnings"])
            kind, confidence = "booking_plan", 0.75 if meta.get("used_fallback") else 0.9
        else:
            decision, reasoning = f"asked {result['rider_id']}: {result['question']}", f"missing {result['field']}"
            kind, confidence = "booking_question", 0.5
        reasoning = f"answered by {meta.get('label', RULE_BASED)}: {reasoning}"
        try:
            await self.record(AGENT, text[:300], decision, reasoning[:600], confidence, result.get("latency_ms", 0),
                              bool(meta.get("used_fallback")), meta.get("fallback_reason"),
                              {"kind": kind, "rider_id": result["rider_id"], "plan_id": result.get("plan_id"),
                               "question_id": result.get("question_id"), "reply_to": reply_to,
                               "model": meta.get("model"), "tokens": meta.get("tokens", 0),
                               "cached": meta.get("cached", False)})
        except Exception as err:  # noqa: BLE001 - auditing must never break booking
            log.warning("booking audit failed: %s", err)

    async def confirm(self, plan_id: str) -> Dict[str, Any]:
        if plan_id in self.confirmed:
            return self.confirmed[plan_id]
        plan = self.plans.get(plan_id)
        if plan is None:
            raise PlanNotFound(plan_id)
        started = time.perf_counter()
        payload = {
            "plan_id": plan_id, "rider_id": plan["rider_id"],
            "legs": [{
                "seq": lg["seq"], "passenger": lg["passenger"], "passenger_name": lg["passenger_name"],
                "tier": lg["tier"], "pickup": {"lat": lg["origin"]["lat"], "lng": lg["origin"]["lng"]},
                "dropoff": {"lat": lg["destination"]["lat"], "lng": lg["destination"]["lng"]},
                "pickup_name": lg["origin"]["label"], "dropoff_name": lg["destination"]["label"],
                "pickup_at": lg["pickup_at"], "arrive_by": lg["arrive_by"], "fare": lg["price"]["total"],
                "duration_min": lg["duration_min"],
            } for lg in plan["legs"]],
        }
        res = await self.gateway.schedule(payload)  # GatewayError -> caller answers 502
        scheduled = res.get("legs") or []
        out = {"plan_id": plan_id, "status": "scheduled", "rider_id": plan["rider_id"],
               "scheduled_ride_ids": [lg.get("id") for lg in scheduled], "legs": scheduled,
               "demo_mode": bool(res.get("demo_mode", self.demo))}
        self.confirmed[plan_id] = out
        meta = plan.get("answered_by") or {}
        try:
            await self.record(AGENT, f"confirm {plan_id}", f"scheduled {len(scheduled)} rides for {plan['rider_id']}",
                              "rides handed to the dispatch-go scheduler: " + ", ".join(out["scheduled_ride_ids"]),
                              1.0, _ms(started), bool(meta.get("used_fallback")), meta.get("fallback_reason"),
                              {"kind": "booking_confirm", "plan_id": plan_id, "rider_id": plan["rider_id"],
                               "model": meta.get("model")})
        except Exception as err:  # noqa: BLE001
            log.warning("booking audit failed: %s", err)
        return out


_agent: Optional[BookingAgent] = None


def get_booking_agent() -> BookingAgent:
    global _agent
    if _agent is None:
        _agent = BookingAgent()
    return _agent


def reset_booking_agent() -> None:
    global _agent
    _agent = None


__all__ = ["BookingAgent", "GatewayError", "PlanNotFound", "QuestionNotFound", "get_booking_agent",
           "reset_booking_agent", "AGENT"]
