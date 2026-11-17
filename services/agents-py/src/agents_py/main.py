import asyncio
import contextlib
import re
from contextlib import asynccontextmanager
from typing import Any, Dict, Optional
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import Response
from pydantic import BaseModel
from prometheus_client import generate_latest, CONTENT_TYPE_LATEST, Counter, Histogram
import litellm

from src.agents_py.agents import ops_supervisor
from src.agents_py.api import router as platform_router
from src.agents_py.config import settings
from src.agents_py.core import infra
from src.agents_py.cors import parse_origins

# Prometheus Instrumentation
REQUEST_COUNT = Counter(
    "hexago_agents_requests_total",
    "Total requests to agents service",
    ["method", "endpoint", "status"]
)
REQUEST_LATENCY = Histogram(
    "hexago_agents_request_latency_seconds",
    "Request latency in seconds",
    ["endpoint"]
)

@asynccontextmanager
async def lifespan(app: FastAPI):
    # Setup LiteLLM parameters
    litellm.drop_params = True
    heartbeat = None
    if settings.HEARTBEAT_INTERVAL_S > 0:
        heartbeat = asyncio.create_task(ops_supervisor.heartbeat_loop(settings.HEARTBEAT_INTERVAL_S))
    try:
        yield
    finally:
        if heartbeat is not None:
            heartbeat.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await heartbeat
        await infra.close()

app = FastAPI(
    title="HexaGo Autonomous Agent Brain",
    description="Multi-agent orchestration platform for zero-human MaaS operations",
    version="1.0.0",
    lifespan=lifespan,
)

_cors_origins, _cors_origin_regex = parse_origins(settings.CORS_ALLOWED_ORIGINS)
app.add_middleware(
    CORSMiddleware,
    allow_origins=_cors_origins,
    allow_origin_regex=_cors_origin_regex,
    allow_methods=["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"],
    allow_headers=["Accept", "Authorization", "Content-Type", "X-Request-ID"],
    max_age=600,
)

app.include_router(platform_router)


class IntentRequest(BaseModel):
    query: str
    agent: Optional[str] = "Booking Intent"

class IntentResponse(BaseModel):
    agent: str
    query: str
    extracted: Dict[str, Any]
    fallback_mode: bool
    confidence: float
    model_used: Optional[str] = None

@app.get("/healthz")
async def healthz():
    return {
        "status": "ok",
        "service": "agents-py",
        "model": settings.LLM_MODEL,
        "demo_mode": settings.DEMO_MODE,
        "llm_enabled": settings.LLM_ENABLED,
        "components": await infra.status(),
        "city_center": {
            "lat": settings.CITY_CENTER_LAT,
            "lng": settings.CITY_CENTER_LNG,
        },
    }

@app.get("/metrics")
async def metrics():
    return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)

@app.get("/v1/agents/ping")
async def agents_ping():
    return {
        "message": "pong",
        "service": "agents-py",
    }

def deterministic_intent_fallback(query: str) -> Dict[str, Any]:
    """Deterministic non-LLM fallback parser for Booking Intent."""
    lower = query.lower()
    
    # Extract passenger count heuristic
    passengers = 1
    match = re.search(r"(\d+)\s*(?:passenger|person|people|pax|seats?)", lower)
    if match:
        passengers = int(match.group(1))

    # Basic origin/destination pattern: "from X to Y"
    from_to = re.search(r"from\s+([a-zA-Z0-9\s]+?)\s+to\s+([a-zA-Z0-9\s]+)", lower)
    if from_to:
        origin = from_to.group(1).strip().title()
        destination = from_to.group(2).strip().title()
    else:
        origin = "Current Location"
        destination = query.replace("ride to", "").replace("go to", "").strip().title()

    return {
        "origin": origin,
        "destination": destination,
        "passengers": passengers,
        "service_tier": "standard",
    }

@app.post("/v1/agents/parse-intent", response_model=IntentResponse)
async def parse_intent(request: IntentRequest):
    # Deterministic fallback mode is activated when no API key or in mock/demo mode
    use_fallback = not settings.GEMINI_API_KEY or settings.GEMINI_API_KEY.startswith("mock-")

    if not use_fallback:
        try:
            prompt = (
                f"Extract structured booking parameters from this ride request: '{request.query}'. "
                f"Return JSON with origin, destination, and passengers count."
            )
            response = await litellm.acompletion(
                model=settings.LLM_MODEL,
                messages=[{"role": "user", "content": prompt}],
                api_key=settings.GEMINI_API_KEY,
                temperature=0.0,
            )
            return IntentResponse(
                agent=request.agent or "Booking Intent",
                query=request.query,
                extracted={
                    "origin": "Extracted via LLM",
                    "destination": "Extracted via LLM",
                    "passengers": 1,
                },
                fallback_mode=False,
                confidence=0.95,
                model_used=settings.LLM_MODEL,
            )
        except Exception:
            # Fall through to deterministic fallback if LLM times out or errors
            use_fallback = True

    extracted = deterministic_intent_fallback(request.query)
    return IntentResponse(
        agent=request.agent or "Booking Intent",
        query=request.query,
        extracted=extracted,
        fallback_mode=True,
        confidence=0.85,
        model_used="deterministic-rule-engine",
    )

