import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.cors import DEFAULT_ORIGINS, parse_origins
from src.agents_py.main import app


def test_parse_origins_default():
    exact, regex = parse_origins(DEFAULT_ORIGINS)
    assert exact == ["https://hexago-web.onrender.com"]
    assert regex is not None


def test_parse_origins_skips_blanks_and_trailing_slash():
    exact, regex = parse_origins(" https://a.example.com/ , , ")
    assert exact == ["https://a.example.com"]
    assert regex is None


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "origin,allowed",
    [
        ("https://hexago-web.onrender.com", True),
        ("http://localhost:5173", True),
        ("http://localhost", True),
        ("http://127.0.0.1:61234", True),
        ("https://evil.example.com", False),
        ("http://localhost.evil.com", False),
    ],
)
async def test_cors_simple_request(origin, allowed):
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.get("/v1/agents/ping", headers={"Origin": origin})
        assert response.status_code == 200
        assert (response.headers.get("access-control-allow-origin") == origin) is allowed


@pytest.mark.asyncio
async def test_cors_preflight():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.options(
            "/v1/agents/parse-intent",
            headers={
                "Origin": "https://hexago-web.onrender.com",
                "Access-Control-Request-Method": "POST",
                "Access-Control-Request-Headers": "content-type",
            },
        )
        assert response.status_code == 200
        assert response.headers["access-control-allow-origin"] == "https://hexago-web.onrender.com"
        assert "POST" in response.headers["access-control-allow-methods"]
