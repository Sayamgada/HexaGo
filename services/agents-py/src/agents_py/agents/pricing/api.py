"""Pricing endpoints (dispatch-go proxies /v1/pricing/* here).

    POST /v1/pricing/quote   {tier, pickup:{lat,lng}, dropoff:{lat,lng}}  -> fare breakdown
                             or {tier, distance_km, duration_min, supply?, demand?}
    GET  /v1/pricing/tiers   rate card
    GET  /v1/pricing/surge-map  H3 res-8 cells with multiplier + boundary (Surge Equilibrium, every 30 s)
"""
from __future__ import annotations

from typing import Optional

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from src.agents_py.agents.booking.gateway import DispatchGateway
from src.agents_py.agents.booking.itinerary import estimate
from src.agents_py.agents.pricing import surge_map
from src.agents_py.agents.pricing.quote import RATES, SURGE_CAP, CellLoad, quote
from src.agents_py.config import settings

router = APIRouter()


class LatLngIn(BaseModel):
    lat: float = Field(..., ge=-90, le=90)
    lng: float = Field(..., ge=-180, le=180)


class QuoteIn(BaseModel):
    tier: str = "mini"
    pickup: Optional[LatLngIn] = None
    dropoff: Optional[LatLngIn] = None
    distance_km: Optional[float] = Field(None, ge=0, le=500)
    duration_min: Optional[float] = Field(None, ge=0, le=1440)
    supply: Optional[int] = Field(None, ge=0)
    demand: Optional[int] = Field(None, ge=0)


def gateway() -> DispatchGateway:
    return DispatchGateway()


@router.get("/v1/pricing/tiers")
async def tiers():
    return {"currency": "INR", "surge_cap": SURGE_CAP,
            "tiers": {t: {"label": r.label, "base": r.base, "per_km": r.per_km, "per_min": r.per_min,
                          "minimum": r.minimum} for t, r in RATES.items()}}


@router.post("/v1/pricing/quote")
async def price_quote(body: QuoteIn):
    gw = gateway()
    if body.pickup and body.dropoff:
        trip = await estimate(gw.eta, (body.pickup.lat, body.pickup.lng), (body.dropoff.lat, body.dropoff.lng))
        distance, duration, source = trip.distance_km, trip.duration_min, trip.source
    elif body.distance_km is not None and body.duration_min is not None:
        distance, duration, source = body.distance_km, body.duration_min, "request"
    else:
        raise HTTPException(422, "send pickup + dropoff, or distance_km + duration_min")
    if body.supply is not None or body.demand is not None:
        load = CellLoad(supply=body.supply or 0, demand=body.demand or 0, source="request")
    elif body.pickup:
        load = await gw.cell_load(body.pickup.lat, body.pickup.lng)
    else:
        load = CellLoad()
    try:
        q = quote(body.tier, distance, duration, load)
    except ValueError as err:
        raise HTTPException(422, str(err)) from None
    return {**q, "eta_source": source}


@router.get("/v1/pricing/surge-map")
async def get_surge_map():
    """The latest surge map; recomputed here if the 30 s loop has not run lately (e.g. right after a wake-up)."""
    m = surge_map.get_map()
    interval = settings.SURGE_MAP_INTERVAL_S or 30
    if m.stale(2 * interval):
        await surge_map.recompute(m)
    return {**m.view(), "interval_s": interval}
