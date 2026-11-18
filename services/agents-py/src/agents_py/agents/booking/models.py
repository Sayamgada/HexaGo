"""Structured booking request: what the LLM (or the rule-based parser) extracts."""
from __future__ import annotations

import re
from typing import List, Optional

from pydantic import BaseModel, Field, field_validator

_HHMM = re.compile(r"^([01]?\d|2[0-3]):([0-5]\d)$")


def _hhmm(v: Optional[str]) -> Optional[str]:
    if v is None:
        return None
    v = str(v).strip()
    if not v or v.lower() in ("null", "none"):
        return None
    m = _HHMM.match(v)
    if not m:
        raise ValueError(f"time must be HH:MM (24h), got {v!r}")
    return f"{int(m.group(1)):02d}:{m.group(2)}"


class LegSpec(BaseModel):
    """One ride in the rider's own words: places are names, times are local HH:MM."""

    passenger: str = Field("me", description='"me" for the rider, "kid" for their child, or a relation like "mom"')
    passenger_label: Optional[str] = Field(None, description='how the rider referred to them, e.g. "my daughter"')
    origin: Optional[str] = Field(None, description='pickup place as said, e.g. "school", "office", "MG Road"; '
                                                    'null = the rider\'s current location')
    destination: Optional[str] = Field(None, description="drop-off place as said")
    pickup_at: Optional[str] = Field(None, description="pickup time, 24h HH:MM local, or null")
    arrive_by: Optional[str] = Field(None, description="latest arrival time, 24h HH:MM local, or null")
    day_offset: int = Field(0, ge=0, le=7, description="0 = today, 1 = tomorrow")
    tier: Optional[str] = Field(None, description="auto | mini | sedan | premium | xl | av | pool, or null")
    asap: bool = Field(False, description="true when the rider said now / asap")

    _norm_pickup = field_validator("pickup_at", mode="before")(_hhmm)
    _norm_arrive = field_validator("arrive_by", mode="before")(_hhmm)

    @field_validator("passenger", mode="before")
    @classmethod
    def _passenger(cls, v):
        v = (v or "me").strip().lower()
        return {"i": "me", "myself": "me", "self": "me", "rider": "me", "child": "kid", "son": "kid",
                "daughter": "kid", "kids": "kid", "children": "kid"}.get(v, v)

    @field_validator("tier", mode="before")
    @classmethod
    def _tier(cls, v):
        if v is None:
            return None
        v = str(v).strip().lower()
        return v or None


class ParsedRequest(BaseModel):
    legs: List[LegSpec] = Field(default_factory=list)
    tier: Optional[str] = Field(None, description="tier for legs that do not name one")
    ambiguities: List[str] = Field(default_factory=list, description="anything you could not determine")
