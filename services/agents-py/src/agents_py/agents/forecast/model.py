"""Demand Forecast: the pure model.

Ride requests are binned per H3 resolution-8 cell into 5-minute buckets over
the last 2 hours. Each cell's series is smoothed with Holt's linear
exponential smoothing (level + damped trend), scaled by a Bengaluru
time-of-day curve to the horizon, blended with the same hour on previous days
when that history exists, and multiplied by the boost of any active what-if
scenario (rain_storm, concert_exit, rush_hour). Every function here is pure:
the same requests, clock and scenarios give the same cells.
"""
from __future__ import annotations

import datetime as dt
import math
import re
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple

import h3

RESOLUTION = 8
BIN_S = 300                 # 5-minute buckets
WINDOW_S = 7200             # last 2 hours
RECENT_S = 1800             # "last 30 min" in explanations
HISTORY_HALF_S = 1800       # previous days: +-30 min around the same time of day
HISTORY_DAYS = 7
HORIZONS = (0, 15, 30)
MAX_CELLS = 200

ALPHA = 0.3                 # level smoothing
BETA = 0.1                  # trend smoothing
PHI = 0.9                   # trend damping per 5-minute step
HISTORY_WEIGHT = 0.3        # share of the same-hour history in the blend
REF_RATE = 12.0             # rides/h at which demand_score = 0.5

AVG_FARE_INR = 220.0
DRIVER_SHARE = 0.75
MAX_TRIPS_PER_HR = 2.5

RUSH_BOOST = 1.5
RAIN_BASE, RAIN_OUTER, RAIN_RADIUS_KM = 1.4, 0.6, 6.0
CONCERT_PEAK, CONCERT_RADIUS_KM = 2.5, 2.0
FADE_MIN = 10.0             # a scenario's boost decays with this time constant once it ends

DEFAULT_CENTER = (12.9716, 77.5946)
VENUE = (12.9984, 77.5920)  # Palace Grounds, relative to DEFAULT_CENTER
SCENARIO_NAMES = ("rain_storm", "concert_exit", "rush_hour")
IST = dt.timezone(dt.timedelta(hours=5, minutes=30))

# Bengaluru weekday demand by IST hour (piecewise linear; 24 wraps to 0).
PROFILE: Tuple[Tuple[float, float], ...] = (
    (0, 0.25), (5, 0.2), (7, 0.6), (9, 1.4), (11, 0.9), (13, 0.8), (15, 0.85),
    (17, 1.2), (19, 1.5), (21, 1.0), (23, 0.45), (24, 0.25),
)


@dataclass(frozen=True)
class RideRequest:
    lat: float
    lng: float
    at: dt.datetime


@dataclass(frozen=True)
class Scenario:
    name: str
    until: dt.datetime
    lat: Optional[float] = None
    lng: Optional[float] = None


def cell_of(lat: Any, lng: Any, resolution: int = RESOLUTION) -> Optional[str]:
    try:
        la, ln = float(lat), float(lng)
        if not (-90 <= la <= 90 and -180 <= ln <= 180):
            return None
        return h3.latlng_to_cell(la, ln, resolution)
    except (TypeError, ValueError, h3.H3BaseException):
        return None


def bin_requests(reqs: Iterable[RideRequest], now: dt.datetime, window_s: int = WINDOW_S,
                 bin_s: int = BIN_S) -> Dict[str, List[int]]:
    """Per cell: request counts per bucket, oldest first; requests outside the window are skipped."""
    n = window_s // bin_s
    out: Dict[str, List[int]] = {}
    for r in reqs:
        age = (now - r.at).total_seconds()
        if age < 0 or age >= window_s:
            continue
        c = cell_of(r.lat, r.lng)
        if c is None:
            continue
        series = out.setdefault(c, [0] * n)
        series[n - 1 - int(age // bin_s)] += 1
    return out


def holt(series: Sequence[float], alpha: float = ALPHA, beta: float = BETA) -> Tuple[float, float]:
    """Holt's linear smoothing: (level, trend) after the last observation."""
    if not series:
        return 0.0, 0.0
    level, trend = float(series[0]), 0.0
    for x in series[1:]:
        prev = level
        level = alpha * x + (1 - alpha) * (level + trend)
        trend = beta * (level - prev) + (1 - beta) * trend
    return level, trend


def smooth_forecast(series: Sequence[float], steps: int, phi: float = PHI) -> float:
    """Counts per bucket expected `steps` buckets ahead (damped trend, never negative)."""
    level, trend = holt(series)
    damp = sum(phi ** i for i in range(1, steps + 1))
    return max(0.0, level + damp * trend)


def ist_hour(t: dt.datetime) -> float:
    local = t.astimezone(IST)
    return local.hour + local.minute / 60 + local.second / 3600


def hourly_profile(hour: float) -> float:
    h = hour % 24
    for (h0, v0), (h1, v1) in zip(PROFILE, PROFILE[1:]):
        if h0 <= h <= h1:
            return v0 + (v1 - v0) * (h - h0) / (h1 - h0)
    return PROFILE[0][1]  # pragma: no cover - h is always inside [0, 24)


def seasonality(now: dt.datetime, horizon_min: int) -> float:
    """Time-of-day multiplier from now to now + horizon."""
    return hourly_profile(ist_hour(now + dt.timedelta(minutes=horizon_min))) / hourly_profile(ist_hour(now))


def history_rates(history: Iterable[RideRequest], now: dt.datetime,
                  half_s: int = HISTORY_HALF_S) -> Tuple[Dict[str, float], int]:
    """Same time of day on previous days: rides/h per cell averaged over the days that have data."""
    counts: Dict[str, int] = {}
    days = set()
    for r in history:
        age_s = (now - r.at).total_seconds()
        day = round(age_s / 86400)
        if day < 1 or abs(age_s - day * 86400) > half_s:
            continue
        c = cell_of(r.lat, r.lng)
        if c is None:
            continue
        days.add(day)
        counts[c] = counts.get(c, 0) + 1
    if not days:
        return {}, 0
    hours = 2 * half_s / 3600
    return {c: n / len(days) / hours for c, n in counts.items()}, len(days)


def _km(a: Tuple[float, float], b: Tuple[float, float]) -> float:
    return h3.great_circle_distance(a, b, unit="km")


def venue_for(s: Scenario, center: Tuple[float, float]) -> Tuple[float, float]:
    if s.lat is not None and s.lng is not None:
        return s.lat, s.lng
    return VENUE[0] + center[0] - DEFAULT_CENTER[0], VENUE[1] + center[1] - DEFAULT_CENTER[1]


def scenario_boost(s: Scenario, lat: float, lng: float, center: Tuple[float, float]) -> float:
    if s.name == "rain_storm":  # outer areas lose two-wheelers and walkers first
        return RAIN_BASE + RAIN_OUTER * min(_km((lat, lng), center) / RAIN_RADIUS_KM, 1.0)
    if s.name == "concert_exit":
        d = _km((lat, lng), venue_for(s, center))
        return 1.0 + CONCERT_PEAK * max(0.0, 1 - d / CONCERT_RADIUS_KM)
    if s.name == "rush_hour":
        return RUSH_BOOST
    return 1.0


def boost_for(scenarios: Sequence[Scenario], lat: float, lng: float, horizon_min: int, now: dt.datetime,
              center: Tuple[float, float] = DEFAULT_CENTER) -> float:
    """Product of the active scenarios' boosts at now + horizon (fading after each ends)."""
    at = now + dt.timedelta(minutes=horizon_min)
    total = 1.0
    for s in scenarios:
        b = scenario_boost(s, lat, lng, center)
        past = (at - s.until).total_seconds() / 60
        if past > 0:
            b = 1.0 + (b - 1.0) * math.exp(-past / FADE_MIN)
        total *= b
    return total


def _parse_ts(raw: Any) -> Optional[dt.datetime]:
    if not isinstance(raw, str):
        return None
    s = re.sub(r"(\.\d{6})\d+", r"\1", raw.strip())  # Go writes nanoseconds
    try:
        t = dt.datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None
    return t if t.tzinfo else t.replace(tzinfo=dt.timezone.utc)


def parse_scenarios(cfg: Optional[Dict[str, Any]]) -> List[Scenario]:
    """Active forecast-relevant scenarios from dispatch-go's GET /v1/sim/config."""
    cfg = cfg or {}
    venue = next((sp for sp in cfg.get("spikes") or [] if str(sp.get("name", "")) == "concert exit"), None)
    out = []
    for raw in cfg.get("scenarios") or []:
        name, until = raw.get("name"), _parse_ts(raw.get("active_until"))
        if name not in SCENARIO_NAMES or until is None:
            continue
        lat = lng = None
        if name == "concert_exit" and venue is not None:
            lat, lng = float(venue["lat"]), float(venue["lng"])
        out.append(Scenario(name, until, lat, lng))
    return out


def supply_by_cell(snapshot: Optional[Dict[str, Any]]) -> Dict[str, int]:
    """Idle cars per cell from dispatch-go's supply snapshot."""
    out: Dict[str, int] = {}
    for d in (snapshot or {}).get("drivers") or []:
        if d.get("status") != "idle":
            continue
        c = cell_of(d.get("lat"), d.get("lng"))
        if c is not None:
            out[c] = out.get(c, 0) + 1
    return out


def demand_score(rate_per_hr: float) -> float:
    """0..1 on an absolute scale (REF_RATE rides/h = 0.5), so a city-wide rise shows on the map."""
    return round(rate_per_hr / (rate_per_hr + REF_RATE), 4) if rate_per_hr > 0 else 0.0


def expected_earnings(rate_per_hr: float, supply: int) -> int:
    """INR per hour for one more driver here: trips shared with the idle cars already there."""
    trips = min(MAX_TRIPS_PER_HR, rate_per_hr / (max(supply, 0) + 1))
    return round(AVG_FARE_INR * DRIVER_SHARE * trips)


def _trend(level: float, trend: float) -> str:
    if trend > 0.05 * max(level, 1.0):
        return "rising"
    if trend < -0.05 * max(level, 1.0):
        return "falling"
    return "steady"


SCENARIO_PHRASES = {
    "rain_storm": "rain storm pushing riders off two-wheelers",
    "concert_exit": "concert crowd leaving Palace Grounds",
    "rush_hour": "rush hour",
}


def template_explanation(c: Dict[str, Any], horizon: int, scenarios: Sequence[Scenario]) -> str:
    when = "now" if horizon == 0 else f"in +{horizon} min"
    trend = {"rising": "demand building", "falling": "easing"}.get(c["trend"], "steady")
    parts = [f"{c['recent_requests']} requests in the last 30 min, {trend}"]
    if c.get("history_rate", 0) > 0:
        parts.append(f"same hour on previous days: {c['history_rate']:.0f}/h")
    parts += [SCENARIO_PHRASES[s.name] for s in scenarios if s.name in SCENARIO_PHRASES]
    return (f"~{c['expected_requests_per_hr']:.0f} rides/h expected {when} ({'; '.join(parts)}); "
            f"about ₹{c['expected_earnings_per_hr']}/h for a driver here.")


def forecast(recent: Iterable[RideRequest], history: Iterable[RideRequest], now: dt.datetime,
             scenarios: Sequence[Scenario] = (), supply: Optional[Dict[str, int]] = None,
             center: Tuple[float, float] = DEFAULT_CENTER, max_cells: int = MAX_CELLS) -> Dict[int, List[Dict[str, Any]]]:
    """Cells per horizon (0 / 15 / 30 min), highest demand first."""
    recent = list(recent)
    supply = supply or {}
    bins = bin_requests(recent, now)
    hist, _ = history_rates(history, now)
    running = [s for s in scenarios if s.until > now]  # an ended scenario still fades the numbers, not the text
    recent_bins = RECENT_S // BIN_S
    out: Dict[int, List[Dict[str, Any]]] = {}
    for horizon in HORIZONS:
        cells = []
        for c in sorted(set(bins) | set(hist)):
            series = bins.get(c, [])
            level, tr = holt(series)
            per_hr = smooth_forecast(series, horizon * 60 // BIN_S) * 3600 / BIN_S * seasonality(now, horizon)
            if c in hist:
                per_hr = (1 - HISTORY_WEIGHT) * per_hr + HISTORY_WEIGHT * hist[c]
            lat, lng = h3.cell_to_latlng(c)
            rate = per_hr * boost_for(scenarios, lat, lng, horizon, now, center)
            sup = supply.get(c, 0)
            row = {
                "h3": c,
                "center": {"lat": round(lat, 6), "lng": round(lng, 6)},
                "boundary": [[round(a, 6), round(b, 6)] for a, b in h3.cell_to_boundary(c)],
                "demand_score": demand_score(rate),
                "expected_requests_per_hr": round(rate, 1),
                "expected_earnings_per_hr": expected_earnings(rate, sup),
                "supply": sup,
                "recent_requests": sum(series[-recent_bins:]),
                "trend": _trend(level, tr),
                "history_rate": round(hist.get(c, 0.0), 2),
            }
            row["explanation"] = template_explanation(row, horizon, running)
            row["explanation_source"] = "template"
            cells.append(row)
        cells.sort(key=lambda r: (-r["demand_score"], r["h3"]))
        out[horizon] = cells[:max_cells]
    return out


def hotspots(cells: Sequence[Dict[str, Any]], n: int = 5) -> List[Dict[str, Any]]:
    """Where one more driver earns most: expected earnings/h, then demand."""
    return sorted(cells, key=lambda r: (-r["expected_earnings_per_hr"], -r["demand_score"], r["h3"]))[:n]
