import pytest
from httpx import ASGITransport, AsyncClient
from src.agents_py.main import app

@pytest.mark.asyncio
async def test_healthz():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.get("/healthz")
        assert response.status_code == 200
        data = response.json()
        assert data["status"] == "ok"
        assert data["service"] == "agents-py"
        assert "city_center" in data
        assert data["city_center"]["lat"] == 12.9716
        assert data["city_center"]["lng"] == 77.5946

@pytest.mark.asyncio
async def test_metrics():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.get("/metrics")
        assert response.status_code == 200
        assert b"python_info" in response.content or b"process_" in response.content or b"http_" in response.content

@pytest.mark.asyncio
async def test_agents_ping():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.get("/v1/agents/ping")
        assert response.status_code == 200
        data = response.json()
        assert data["message"] == "pong"
        assert data["service"] == "agents-py"

@pytest.mark.asyncio
async def test_agent_fallback_parsing():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        payload = {
            "query": "Need a ride from Koramangala to Indiranagar for 2 passengers",
            "agent": "Booking Intent"
        }
        response = await client.post("/v1/agents/parse-intent", json=payload)
        assert response.status_code == 200
        data = response.json()
        assert data["agent"] == "Booking Intent"
        assert "passengers" in data["extracted"]
        assert data["fallback_mode"] in (True, False)
