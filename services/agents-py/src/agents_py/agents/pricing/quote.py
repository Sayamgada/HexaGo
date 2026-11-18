"""Fare quotes: base + per-km + per-minute by tier, times a surge multiplier.

The surge multiplier comes from a `SurgeModel`. Today that is `RatioSurge`, a
pure function of the pickup H3 cell's supply/demand ratio capped at 2.5x; the
Surge Equilibrium agent will replace it later without touching `quote`.

All money is in INR, rounded to whole rupees for the rider-facing total.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, Protocol

CURRENCY = "INR"
SURGE_CAP = 2.5
SURGE_SLOPE = 0.5  # +0.5x for every extra request per idle car beyond 1:1


@dataclass(frozen=True)
class TierRate:
    base: float
    per_km: float
    per_min: float
    minimum: float
    label: str


RATES: Dict[str, TierRate] = {
    "auto": TierRate(30, 15, 1.0, 40, "Auto"),
    "mini": TierRate(50, 18, 1.5, 80, "Mini"),
    "sedan": TierRate(70, 22, 2.0, 120, "Sedan"),
    "premium": TierRate(120, 30, 3.0, 200, "Premium sedan"),
    "xl": TierRate(100, 28, 2.5, 180, "XL"),
    "av": TierRate(80, 20, 1.5, 120, "Autonomous"),
    "pool": TierRate(40, 12, 1.0, 60, "Pool"),
}
TIERS = tuple(RATES)


@dataclass(frozen=True)
class CellLoad:
    """Supply (idle cars) and demand (busy cars + open requests) around a pickup cell."""

    cell: str = ""
    supply: int = 0
    demand: int = 0
    source: str = "none"


class SurgeModel(Protocol):
    name: str

    def multiplier(self, load: CellLoad) -> float: ...


def ratio_surge(load: CellLoad, cap: float = SURGE_CAP) -> float:
    """1.0 while demand <= supply, then +0.5x per unit of demand/supply above 1, capped."""
    if load.demand <= 0:
        return 1.0
    if load.supply <= 0:
        return cap
    ratio = load.demand / load.supply
    if ratio <= 1:
        return 1.0
    return round(min(cap, 1 + SURGE_SLOPE * (ratio - 1)), 2)


class RatioSurge:
    name = "supply_demand_ratio"

    def __init__(self, cap: float = SURGE_CAP):
        self.cap = cap

    def multiplier(self, load: CellLoad) -> float:
        return ratio_surge(load, self.cap)


def normalize_tier(tier: str) -> str:
    t = (tier or "").strip().lower()
    if t not in RATES:
        raise ValueError(f"unknown tier {tier!r}; expected one of {', '.join(TIERS)}")
    return t


def quote(tier: str, distance_km: float, duration_min: float, load: CellLoad = CellLoad(),
          surge: SurgeModel = RatioSurge()) -> Dict[str, Any]:
    """The fare breakdown for one ride."""
    t = normalize_tier(tier)
    if distance_km < 0 or duration_min < 0:
        raise ValueError("distance_km and duration_min must be >= 0")
    r = RATES[t]
    distance_fare = r.per_km * distance_km
    time_fare = r.per_min * duration_min
    subtotal = max(r.minimum, r.base + distance_fare + time_fare)
    multiplier = surge.multiplier(load)
    total = round(subtotal * multiplier)
    return {
        "tier": t,
        "tier_label": r.label,
        "currency": CURRENCY,
        "distance_km": round(distance_km, 2),
        "duration_min": round(duration_min, 1),
        "base": r.base,
        "distance_fare": round(distance_fare, 2),
        "time_fare": round(time_fare, 2),
        "minimum_fare": r.minimum,
        "minimum_applied": subtotal == r.minimum and r.base + distance_fare + time_fare < r.minimum,
        "subtotal": round(subtotal, 2),
        "surge_multiplier": multiplier,
        "surge_amount": round(total - subtotal, 2),
        "surge_model": getattr(surge, "name", type(surge).__name__),
        "cell": {"id": load.cell, "supply": load.supply, "demand": load.demand, "source": load.source},
        "total": total,
    }
