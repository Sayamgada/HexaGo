"""Booking and pricing HTTP endpoints."""
import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents.booking import api as booking_api
from src.agents_py.agents.booking import planner
from src.agents_py.agents.booking.gateway import GatewayError
from src.agents_py.agents.booking.itinerary import TripEstimate
from src.agents_py.agents.pricing import api as pricing_api
from src.agents_py.agents.pricing.quote import CellLoad
from src.agents_py.config import settings
from src.agents_py.core import registry
from src.agents_py.main import app
from tests.booking_fakes import GOLDEN_SENTENCE, FakeGateway, make_agent
from tests.helpers import make_llm


@pytest.fixture
def client():
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://t")


@pytest.fixture
def booking(monkeypatch):
    agent, rec = make_agent(llm=make_llm(enabled=False))
    monkeypatch.setattr(planner, "_agent", agent)
    return agent, rec


async def test_personas(client):
    r = await client.get("/v1/agents/booking/personas")
    assert r.status_code == 200
    ids = [p["id"] for p in r.json()["personas"]]
    assert ids == ["rider_priya", "rider_arjun", "rider_meera"]


async def test_parse_confirm_flow(client, booking):
    agent, rec = booking
    r = await client.post("/v1/agents/booking/parse", json={"rider_id": "rider_priya", "text": GOLDEN_SENTENCE})
    assert r.status_code == 200
    plan = r.json()
    assert plan["type"] == "plan" and len(plan["legs"]) == 2 and plan["latency_ms"] >= 0
    r = await client.post("/v1/agents/booking/confirm", json={"plan_id": plan["plan_id"]})
    assert r.status_code == 200 and r.json()["scheduled_ride_ids"] == ["sch_1", "sch_2"]
    assert (await client.post("/v1/agents/booking/confirm", json={"plan_id": "nope"})).status_code == 404
    assert registry.status_of("booking_intent") == "active"


async def test_force_fallback_only_in_demo_mode(client, booking, monkeypatch):
    agent, rec = booking
    r = await client.post("/v1/agents/booking/parse?force_fallback=true",
                          json={"rider_id": "rider_priya", "text": "from home to office at 9 am"})
    assert r.json()["answered_by"]["fallback_reason"] == "forced"
    r = await client.post("/v1/agents/booking/parse", headers={"X-Force-Fallback": "1"},
                          json={"rider_id": "rider_priya", "text": "from home to office at 9 am"})
    assert r.json()["answered_by"]["fallback_reason"] == "forced"
    monkeypatch.setattr(settings, "DEMO_MODE", False)
    r = await client.post("/v1/agents/booking/parse?force_fallback=true",
                          json={"rider_id": "rider_priya", "text": "from home to office at 9 am"})
    assert r.status_code == 403


async def test_question_reply_and_expired_question(client, booking):
    r = await client.post("/v1/agents/booking/parse", json={"rider_id": "rider_arjun",
                                                            "text": "from school to home at 4 pm"})
    q = r.json()
    assert q["type"] == "question"
    r = await client.post("/v1/agents/booking/parse", json={"rider_id": "rider_arjun", "text": "Kabir's school",
                                                            "reply_to": q["question_id"]})
    assert r.json()["type"] == "plan"
    r = await client.post("/v1/agents/booking/parse", json={"rider_id": "rider_arjun", "text": "x",
                                                            "reply_to": q["question_id"]})
    assert r.status_code == 404
    assert (await client.post("/v1/agents/booking/parse", json={"text": ""})).status_code == 422


async def test_confirm_gateway_error_is_502(client, monkeypatch):
    agent, _ = make_agent(llm=make_llm(enabled=False), gateway=FakeGateway(schedule_error=GatewayError("down")))
    monkeypatch.setattr(planner, "_agent", agent)
    plan = (await client.post("/v1/agents/booking/parse", json={"text": "from home to office at 9 am"})).json()
    r = await client.post("/v1/agents/booking/confirm", json={"plan_id": plan["plan_id"]})
    assert r.status_code == 502 and "could not schedule" in r.json()["error"]


# ---- voice ----

async def test_voice_needs_client_side_transcription_for_gemini(client):
    r = await client.post("/v1/agents/booking/voice", content=b"RIFF....", headers={"Content-Type": "audio/wav"})
    assert r.status_code == 400 and r.json()["hint"] == "use client-side transcription"
    assert (await client.post("/v1/agents/booking/voice", content=b"",
                              headers={"Content-Type": "audio/wav"})).status_code == 400
    assert (await client.post("/v1/agents/booking/voice", content=b"x",
                              headers={"Content-Type": "text/plain"})).status_code == 415


async def test_voice_too_large(client, monkeypatch):
    monkeypatch.setattr(booking_api, "MAX_AUDIO_BYTES", 3)
    r = await client.post("/v1/agents/booking/voice", content=b"abcd", headers={"Content-Type": "audio/webm"})
    assert r.status_code == 413


async def test_voice_transcribes_with_a_supported_provider(client, monkeypatch):
    llm = make_llm(model="openai/gpt-4o-mini", api_key=None)

    async def fake_llm():
        return llm

    seen = {}

    async def fake_transcription(model, file):
        seen["model"], seen["name"] = model, file[0]
        return {"text": " Take me to the airport "}

    monkeypatch.setattr(booking_api, "get_llm", fake_llm)
    monkeypatch.setattr(booking_api.litellm, "atranscription", fake_transcription)
    r = await client.post("/v1/agents/booking/voice", content=b"ogg", headers={"Content-Type": "audio/ogg"})
    assert r.json() == {"transcript": "Take me to the airport", "model": "openai/whisper-1"}
    assert seen == {"model": "openai/whisper-1", "name": "audio.ogg"}
    # second call inside the min interval is blocked by the guardrail -> 400
    r = await client.post("/v1/agents/booking/voice", content=b"ogg", headers={"Content-Type": "audio/ogg"})
    assert r.status_code == 400 and r.json()["hint"] == "use client-side transcription"


def test_transcription_model():
    assert booking_api.transcription_model("groq/llama-3") == "groq/whisper-large-v3"
    assert booking_api.transcription_model("gemini/gemini-3.5-flash-lite") is None


# ---- pricing ----

class QuoteGateway:
    async def eta(self, a, b):
        return TripEstimate(10.0, 30.0, "dispatch-go")

    async def cell_load(self, lat, lng):
        return CellLoad("c9", supply=5, demand=10, source="dispatch-go")


async def test_pricing_quote_by_points(client, monkeypatch):
    monkeypatch.setattr(pricing_api, "gateway", lambda: QuoteGateway())
    r = await client.post("/v1/pricing/quote", json={"tier": "sedan", "pickup": {"lat": 12.97, "lng": 77.59},
                                                     "dropoff": {"lat": 13.0, "lng": 77.6}})
    q = r.json()
    assert r.status_code == 200 and q["eta_source"] == "dispatch-go"
    assert q["subtotal"] == 350 and q["surge_multiplier"] == 1.5 and q["total"] == 525
    assert q["cell"]["id"] == "c9"


async def test_pricing_quote_by_distance_and_errors(client, monkeypatch):
    monkeypatch.setattr(pricing_api, "gateway", lambda: QuoteGateway())
    r = await client.post("/v1/pricing/quote", json={"tier": "mini", "distance_km": 10, "duration_min": 30})
    assert r.json()["total"] == 275 and r.json()["surge_multiplier"] == 1.0
    r = await client.post("/v1/pricing/quote", json={"tier": "mini", "distance_km": 10, "duration_min": 30,
                                                     "supply": 1, "demand": 9})
    assert r.json()["surge_multiplier"] == 2.5
    assert (await client.post("/v1/pricing/quote", json={"tier": "mini"})).status_code == 422
    r = await client.post("/v1/pricing/quote", json={"tier": "jet", "distance_km": 1, "duration_min": 1})
    assert r.status_code == 422 and "unknown tier" in r.json()["detail"]
    t = (await client.get("/v1/pricing/tiers")).json()
    assert t["surge_cap"] == 2.5 and set(t["tiers"]) == {"auto", "mini", "sedan", "premium", "xl", "av", "pool"}


async def test_pricing_quote_unreachable_dispatch_uses_haversine(client, monkeypatch):
    monkeypatch.setattr(settings, "DISPATCH_URL", "http://127.0.0.1:9")
    pricing_api.gateway()  # default client is constructible
    r = await client.post("/v1/pricing/quote", json={
        "tier": "auto", "pickup": {"lat": 12.97, "lng": 77.59}, "dropoff": {"lat": 12.98, "lng": 77.6}})
    assert r.status_code == 200
    assert r.json()["eta_source"] == "haversine" and r.json()["cell"]["source"] == "unavailable"

