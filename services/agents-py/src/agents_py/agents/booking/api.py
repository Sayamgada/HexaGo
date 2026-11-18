"""Booking Intent endpoints (reached through the gateway's /v1/agents/* proxy).

    GET  /v1/agents/booking/personas        demo rider personas and their saved places
    POST /v1/agents/booking/parse           {rider_id, text, reply_to?} -> plan | question
                                            ?force_fallback=true (DEMO_MODE only) skips the LLM
    POST /v1/agents/booking/confirm         {plan_id} -> scheduled ride ids (dispatch-go scheduler)
    POST /v1/agents/booking/voice           raw audio body (audio/*) -> transcript, or 400 when the
                                            configured LLM provider has no transcription API
"""
from __future__ import annotations

import asyncio
from typing import Optional

import litellm
from fastapi import APIRouter, Header, HTTPException, Query, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from src.agents_py.agents.booking.gateway import GatewayError
from src.agents_py.agents.booking.places import PERSONAS
from src.agents_py.agents.booking.planner import (
    AGENT, PlanNotFound, QuestionNotFound, get_booking_agent,
)
from src.agents_py.config import settings
from src.agents_py.core.graph import agent
from src.agents_py.core.llm import get_llm

router = APIRouter()

MAX_AUDIO_BYTES = 10 * 1024 * 1024
# LiteLLM providers with an audio transcription endpoint.
TRANSCRIPTION_PROVIDERS = {"openai": "whisper-1", "azure": "whisper", "groq": "whisper-large-v3"}
CLIENT_SIDE = "use client-side transcription"


class ParseIn(BaseModel):
    rider_id: str = Field("", max_length=64)
    text: str = Field(..., min_length=1, max_length=1000)
    reply_to: Optional[str] = Field(None, max_length=64)


class ConfirmIn(BaseModel):
    plan_id: str = Field(..., min_length=1, max_length=64)


def _flag(v: Optional[str]) -> bool:
    return (v or "").strip().lower() in ("1", "true", "yes", "on")


@agent(AGENT, "Turns one sentence (typed or spoken) into a priced, timed multi-leg ride plan; LLM parse with a "
              "rule-based fallback, saved places + Nominatim, backward ETA timing, scheduled dispatch on confirm.")
async def plan_booking(rider_id: str, text: str, force_fallback: bool = False, reply_to: Optional[str] = None):
    return await get_booking_agent().parse(rider_id, text, force_fallback=force_fallback, reply_to=reply_to)


@router.get("/v1/agents/booking/personas")
async def personas():
    return {"personas": [p.public() for p in PERSONAS.values()], "demo_mode": settings.DEMO_MODE}


@router.post("/v1/agents/booking/parse")
async def parse(body: ParseIn, force_fallback: Optional[str] = Query(None),
                x_force_fallback: Optional[str] = Header(None)):
    forced = _flag(force_fallback) or _flag(x_force_fallback)
    if forced and not settings.DEMO_MODE:
        raise HTTPException(403, "force_fallback is only available in DEMO_MODE")
    try:
        return await plan_booking(body.rider_id, body.text.strip(), forced, body.reply_to)
    except QuestionNotFound:
        raise HTTPException(404, "that question has expired; please send the request again") from None


@router.post("/v1/agents/booking/confirm")
async def confirm(body: ConfirmIn):
    try:
        return await get_booking_agent().confirm(body.plan_id)
    except PlanNotFound:
        raise HTTPException(404, "plan not found or expired") from None
    except GatewayError as err:
        return JSONResponse(status_code=502, content={"error": f"could not schedule the rides: {err}"})


def transcription_model(model: str) -> Optional[str]:
    provider = (model or "").split("/", 1)[0]
    name = TRANSCRIPTION_PROVIDERS.get(provider)
    return f"{provider}/{name}" if name else None


@router.post("/v1/agents/booking/voice")
async def voice(request: Request):
    audio = await request.body()
    ctype = request.headers.get("content-type", "")
    if not audio:
        raise HTTPException(400, "empty audio upload")
    if len(audio) > MAX_AUDIO_BYTES:
        raise HTTPException(413, "audio larger than 10 MB")
    if not (ctype.startswith("audio/") or ctype.startswith("application/octet-stream")):
        raise HTTPException(415, "send the recording as the raw body with an audio/* content type")
    llm = await get_llm()
    model = transcription_model(llm.model)
    if model is None or not llm.enabled:
        return JSONResponse(status_code=400, content={
            "error": f"the configured LLM provider ({llm.model}) has no transcription API; {CLIENT_SIDE}",
            "hint": CLIENT_SIDE})
    try:
        await llm._check_rate("booking_intent:voice")  # same guardrails as completions
        await llm._reserve_call()
        ext = ctype.split("/", 1)[-1].split(";")[0] or "webm"
        resp = await asyncio.wait_for(litellm.atranscription(model=model, file=(f"audio.{ext}", audio)),
                                      llm.timeout_s * 3)
        text = getattr(resp, "text", None) or (resp.get("text") if isinstance(resp, dict) else None)
    except Exception as err:  # noqa: BLE001 - guardrail block, timeout or provider error
        return JSONResponse(status_code=400, content={"error": f"transcription unavailable ({err}); {CLIENT_SIDE}",
                                                      "hint": CLIENT_SIDE})
    return {"transcript": (text or "").strip(), "model": model}
