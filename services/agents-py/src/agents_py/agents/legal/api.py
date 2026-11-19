"""Legal & Jurisdiction endpoints (dispatch-go proxies /v1/legal/* here).

    POST /v1/legal/ingest              {city?, text} -> rule diff (old -> new), applied at once, audited
    GET  /v1/legal/jurisdictions       every city's rules (+ the sample notice for the Ops console)
    GET  /v1/legal/jurisdictions/{city}
    GET  /v1/legal/audit?limit=&city=  rule-change audit log, newest first

Behind LEGAL_ENABLED (default true): off, ingest answers 503 and pricing uses its static cap.
"""
from __future__ import annotations

from typing import Optional

from fastapi import APIRouter, HTTPException, Query
from pydantic import BaseModel, Field

from src.agents_py.agents.legal import legal_agent
from src.agents_py.agents.legal.legal_rules import normalize_city
from src.agents_py.config import settings

router = APIRouter()


class IngestIn(BaseModel):
    city: Optional[str] = Field(None, max_length=64)
    text: str = Field(..., min_length=1, max_length=20_000)


@router.post("/v1/legal/ingest")
async def ingest(body: IngestIn):
    if not settings.LEGAL_ENABLED:
        raise HTTPException(503, "Legal & Jurisdiction agent is disabled (LEGAL_ENABLED=false)")
    try:
        return await legal_agent.ingest(body.text, body.city)
    except legal_agent.UnknownCity as err:
        raise HTTPException(422, str(err)) from None


@router.get("/v1/legal/jurisdictions")
async def jurisdictions():
    a = await legal_agent.get_agent()
    return {"enabled": settings.LEGAL_ENABLED, "jurisdictions": await a.store.list(), "backend": a.store.backend,
            "pricing_city": normalize_city(settings.CITY_NAME), "sample_notice": legal_agent.SAMPLE_NOTICE}


@router.get("/v1/legal/jurisdictions/{city}")
async def jurisdiction(city: str):
    key = normalize_city(city)
    j = await (await legal_agent.get_agent()).store.get(key) if key else None
    if j is None:
        raise HTTPException(404, f"unknown city {city!r}")
    return j


@router.get("/v1/legal/audit")
async def audit(limit: int = Query(50, ge=1, le=500), city: Optional[str] = None):
    key = normalize_city(city) if city else None
    entries = await (await legal_agent.get_agent()).store.audit(limit=limit, city=key or city)
    return {"entries": entries, "count": len(entries)}
