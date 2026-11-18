"""Safety endpoints (dispatch-go proxies /v1/safety/* here, except its own vehicle controls).

    POST /v1/safety/signal                      safety_signal from the dispatch-go detector
    GET  /v1/safety/incidents?limit=            incidents, newest first (+ suppressed false positives)
    GET  /v1/safety/incidents/{id}              timeline, transcript, actions, check-ins, vehicle path
    POST /v1/safety/incidents/{id}/resolve      {"requested_by","note"} - Ops Supervisor must confirm
    GET  /v1/safety/checkins?status=pending     live check-ins (the Ops console's "Open rider phone")
    GET  /v1/safety/checkins/{id}               one check-in (the call screen)
    POST /v1/safety/checkins/{id}/reply         {"transcript"} - the rider's answer
"""
from __future__ import annotations

from typing import Any, Dict, List, Optional

from fastapi import APIRouter, HTTPException, Query
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from src.agents_py.agents.safety.actions import ActionError
from src.agents_py.agents.safety.sentinel import (
    AGENT, CheckinClosed, CheckinNotFound, IncidentNotFound, ResolutionRejected, get_sentinel,
)
from src.agents_py.agents.safety.store import public
from src.agents_py.core.graph import agent

router = APIRouter()


class SignalIn(BaseModel):
    model_config = {"extra": "allow"}

    signal_id: str = ""
    ride_id: str = Field(..., min_length=1, max_length=64)
    driver_id: str = Field(..., min_length=1, max_length=64)
    rider_id: str = ""
    tier: str = ""
    severity: float = Field(..., ge=0, le=1)
    triggers: List[Dict[str, Any]] = Field(default_factory=list)
    location: Optional[Dict[str, Any]] = None


class ReplyIn(BaseModel):
    transcript: str = Field("", max_length=2000)


class ResolveIn(BaseModel):
    requested_by: str = Field("", max_length=64)
    note: str = Field("", max_length=1000)


@agent(AGENT, "Detects route deviations, hard brakes and stops; decides monitor / check-in / escalate "
              "deterministically, calls the rider, escalates to (simulated) emergency services. LLM reasoning is async.")
async def handle_signal(signal: Dict[str, Any]) -> Dict[str, Any]:
    return await (await get_sentinel()).handle_signal(signal)


@router.post("/v1/safety/signal", status_code=202)
async def safety_signal(body: SignalIn):
    return await handle_signal(body.model_dump())


@router.get("/v1/safety/incidents")
async def list_incidents(limit: int = Query(50, ge=1, le=200)):
    s = await get_sentinel()
    incidents = [s.summary(i) for i in await s.store.list_incidents(limit)]
    return {"incidents": incidents, "count": len(incidents), "backend": s.store.backend,
            "suppressed": public(s.store.suppressed[:20]), "pending_checkins": s.pending_checkins()}


@router.get("/v1/safety/incidents/{incident_id}")
async def get_incident(incident_id: str):
    s = await get_sentinel()
    try:
        return await s.detail(incident_id)
    except IncidentNotFound:
        raise HTTPException(404, "incident not found") from None


@router.post("/v1/safety/incidents/{incident_id}/resolve")
async def resolve_incident(incident_id: str, body: ResolveIn):
    s = await get_sentinel()
    try:
        return await s.resolve(incident_id, body.requested_by, body.note)
    except IncidentNotFound:
        raise HTTPException(404, "incident not found") from None
    except ResolutionRejected as err:
        return JSONResponse(status_code=409, content={
            "error": "the Ops Supervisor did not confirm the resolution",
            "violations": list(err.confirmation.violations)})
    except (ActionError, PermissionError) as err:
        return JSONResponse(status_code=502, content={"error": f"could not unfreeze the driver: {err}"})


@router.get("/v1/safety/checkins")
async def list_checkins(status: str = Query("pending")):
    s = await get_sentinel()
    if status == "pending":
        items = s.pending_checkins()
    else:
        items = [s.checkin_view(c) for c in sorted(s.store.checkins.values(), key=lambda c: c["created_at"],
                                                    reverse=True) if status in ("all", c["status"])]
    return {"checkins": items, "count": len(items)}


@router.get("/v1/safety/checkins/{checkin_id}")
async def get_checkin(checkin_id: str):
    s = await get_sentinel()
    c = await s.store.get_checkin(checkin_id)
    if c is None:
        raise HTTPException(404, "check-in not found")
    return s.checkin_view(c)


@router.post("/v1/safety/checkins/{checkin_id}/reply")
async def reply_checkin(checkin_id: str, body: ReplyIn):
    s = await get_sentinel()
    try:
        return await s.handle_reply(checkin_id, body.transcript)
    except CheckinNotFound:
        raise HTTPException(404, "check-in not found") from None
    except CheckinClosed as err:
        return JSONResponse(status_code=409, content={"error": str(err), **s.checkin_view(err.checkin)})

