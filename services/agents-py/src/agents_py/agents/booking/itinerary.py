"""Itinerary timing: order legs, work pickup times backwards from arrive-by, flag conflicts.

Trip estimates come from dispatch-go (GET /v1/eta) with a haversine fallback
using the same speed model: road distance = 1.3 x straight line; the first
6 km at 18 km/h (city traffic), the rest at 40 km/h (arterials / highway).

Rules (all times Asia/Kolkata):

* pickup_at T          -> pickup T, arrival T + trip
* arrive_by A          -> pickup A - trip - ARRIVE_BUFFER_MIN (worked backwards)
* neither              -> chained ``CHAIN_GAP_MIN`` after the same passenger's
                          previous leg, else ``ASAP_LEAD_MIN`` from now
* every explicit time already passed today -> the whole plan moves to tomorrow
* a pickup that has passed, an arrive-by that cannot be met, and a passenger's
  next leg leaving before their previous one ends are reported as warnings.
"""
from __future__ import annotations

import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Awaitable, Callable, List, Optional, Tuple

from src.agents_py.agents.booking.places import haversine_km

log = logging.getLogger("hexago.booking.itinerary")

IST = dt.timezone(dt.timedelta(hours=5, minutes=30), "IST")
ROAD_FACTOR = 1.3
CITY_KM = 6.0
CITY_KMH = 18.0
ARTERIAL_KMH = 40.0
MIN_TRIP_MIN = 3.0
ARRIVE_BUFFER_MIN = 10
CHAIN_GAP_MIN = 5
ASAP_LEAD_MIN = 5
TIGHT_GAP_MIN = 5

LatLng = Tuple[float, float]


@dataclass(frozen=True)
class TripEstimate:
    distance_km: float
    duration_min: float
    source: str  # dispatch-go | haversine


def duration_for_road_km(road_km: float) -> float:
    if road_km <= CITY_KM:
        minutes = road_km / CITY_KMH * 60
    else:
        minutes = CITY_KM / CITY_KMH * 60 + (road_km - CITY_KM) / ARTERIAL_KMH * 60
    return round(max(MIN_TRIP_MIN, minutes), 1)


def haversine_estimate(a: LatLng, b: LatLng) -> TripEstimate:
    road = haversine_km(a, b) * ROAD_FACTOR
    return TripEstimate(round(road, 2), duration_for_road_km(road), "haversine")


EtaFn = Callable[[LatLng, LatLng], Awaitable[TripEstimate]]


async def estimate(eta: Optional[EtaFn], a: LatLng, b: LatLng) -> TripEstimate:
    """dispatch-go's estimate, or the haversine model if it is unavailable."""
    if eta is not None:
        try:
            return await eta(a, b)
        except Exception as err:  # noqa: BLE001 - any failure falls back to haversine
            log.info("eta from dispatch-go unavailable (%s); using haversine", err)
    return haversine_estimate(a, b)


def fmt_time(t: dt.datetime) -> str:
    t = t.astimezone(IST)
    return f"{t.hour % 12 or 12}:{t.minute:02d} {'AM' if t.hour < 12 else 'PM'}"


@dataclass
class LegTiming:
    """What the timing step needs to know about one leg."""

    passenger: str
    origin_label: str
    destination_label: str
    trip: TripEstimate
    pickup_at: Optional[str] = None  # HH:MM
    arrive_by: Optional[str] = None  # HH:MM
    day_offset: int = 0
    asap: bool = False


@dataclass
class TimedLeg:
    index: int  # position in the input
    leg: LegTiming
    pickup: dt.datetime
    arrival: dt.datetime
    arrive_by: Optional[dt.datetime]
    warnings: List[str] = field(default_factory=list)

    @property
    def name(self) -> str:
        return f"{short(self.leg.origin_label)} -> {short(self.leg.destination_label)}"


def short(label: str) -> str:
    """'Office - Manyata Tech Park, Hebbal' -> 'Office'."""
    return (label or "?").split(" - ")[0].strip()


def _at(day: dt.date, hhmm: str, offset: int) -> dt.datetime:
    h, m = (int(x) for x in hhmm.split(":"))
    return dt.datetime.combine(day + dt.timedelta(days=offset), dt.time(h, m), IST)


def plan_times(legs: List[LegTiming], now: dt.datetime) -> Tuple[List[TimedLeg], List[str]]:
    """Timed legs ordered by pickup, and plan-level warnings."""
    now = now.astimezone(IST)
    today = now.date()
    warnings: List[str] = []

    anchors = [_at(today, t, lg.day_offset) for lg in legs for t in (lg.pickup_at, lg.arrive_by) if t]
    shift = 0
    if anchors and all(a < now for a in anchors):
        shift = 1
        warnings.append(f"{fmt_time(min(anchors))} has already passed today, so the plan is for tomorrow.")

    timed: List[TimedLeg] = []
    for i, lg in enumerate(legs):
        trip = dt.timedelta(minutes=lg.trip.duration_min)
        arrive_by = _at(today, lg.arrive_by, lg.day_offset + shift) if lg.arrive_by else None
        notes: List[str] = []
        if lg.pickup_at:
            pickup = _at(today, lg.pickup_at, lg.day_offset + shift)
            if pickup < now:
                notes.append(f"Pickup time {fmt_time(pickup)} has already passed; dispatching as soon as possible.")
                pickup = now + dt.timedelta(minutes=ASAP_LEAD_MIN)
            if arrive_by and pickup + trip > arrive_by:
                notes.append(f"Leaving at {fmt_time(pickup)} arrives about {fmt_time(pickup + trip)}, "
                             f"after the {fmt_time(arrive_by)} arrive-by.")
        elif arrive_by:
            pickup = arrive_by - trip - dt.timedelta(minutes=ARRIVE_BUFFER_MIN)
            earliest = now + dt.timedelta(minutes=ASAP_LEAD_MIN)
            if pickup < earliest:
                pickup = earliest
                if pickup + trip > arrive_by:
                    notes.append(f"Can't arrive by {fmt_time(arrive_by)}: {lg.trip.duration_min:.0f} min trip, "
                                 f"earliest arrival about {fmt_time(pickup + trip)}.")
                else:
                    notes.append(f"Leave now to make {fmt_time(arrive_by)} ({lg.trip.duration_min:.0f} min trip).")
        else:
            prev = next((t for t in reversed(timed) if t.leg.passenger == lg.passenger), None)
            if prev is not None and not lg.asap:
                pickup = prev.arrival + dt.timedelta(minutes=CHAIN_GAP_MIN)
            else:
                pickup = now + dt.timedelta(minutes=ASAP_LEAD_MIN)
        timed.append(TimedLeg(i, lg, pickup, pickup + trip, arrive_by, notes))

    timed.sort(key=lambda t: (t.pickup, t.index))
    warnings.extend(conflicts(timed))
    return timed, warnings


def conflicts(timed: List[TimedLeg]) -> List[str]:
    """A passenger's next leg leaving before (or right as) their previous leg ends."""
    out: List[str] = []
    last: dict = {}
    for t in timed:
        prev = last.get(t.leg.passenger)
        if prev is not None:
            gap = (t.pickup - prev.arrival).total_seconds() / 60
            if gap < 0:
                msg = (f"{prev.name} ends {fmt_time(prev.arrival)}, {t.name} needs {t.leg.trip.duration_min:.0f} min "
                       f"and leaves at {fmt_time(t.pickup)} - overlap.")
                t.warnings.append(msg)
                out.append(msg)
            elif gap < TIGHT_GAP_MIN:
                msg = (f"Tight connection: {prev.name} ends {fmt_time(prev.arrival)} and {t.name} leaves "
                       f"{fmt_time(t.pickup)} ({gap:.0f} min).")
                t.warnings.append(msg)
                out.append(msg)
        last[t.leg.passenger] = t
    return out
