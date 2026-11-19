"""Contract tests: agents-py responses match the public OpenAPI document.

dispatch-go generates the document (``GET /openapi.json``) and commits a copy
at ``docs/openapi.json``; the agents-py endpoints the gateway proxies are held
to it here, with JSON Schema 2020-12 (the OpenAPI 3.1 dialect).
"""
import json
from pathlib import Path

import pytest
from httpx import ASGITransport, AsyncClient
from jsonschema import Draft202012Validator
from referencing import Registry, Resource

from src.agents_py.agents.booking import planner
from src.agents_py.agents.booking.itinerary import TripEstimate
from src.agents_py.agents.pricing import api as pricing_api
from src.agents_py.agents.pricing.quote import CellLoad
from src.agents_py.main import app
from tests.booking_fakes import GOLDEN_SENTENCE, make_agent
from tests.helpers import make_llm

SPEC_PATH = Path(__file__).resolve().parents[3] / "docs" / "openapi.json"
SPEC = json.loads(SPEC_PATH.read_text(encoding="utf-8"))
SPEC_URI = "urn:hexago:openapi"
# The whole document is registered as one opaque resource; "#/components/..."
# refs inside a response schema are rewritten to point into it.
REGISTRY = Registry().with_resource(SPEC_URI, Resource.opaque(SPEC))


def response_schema(method: str, path: str, status: int):
    op = SPEC["paths"][path][method.lower()]
    resp = op["responses"][str(status)]
    if "$ref" in resp:
        resp = SPEC["components"]["responses"][resp["$ref"].rsplit("/", 1)[-1]]
    return resp["content"]["application/json"]["schema"]


def _absolutize(node):
    if isinstance(node, dict):
        return {k: (SPEC_URI + v if k == "$ref" and isinstance(v, str) and v.startswith("#/") else _absolutize(v))
                for k, v in node.items()}
    if isinstance(node, list):
        return [_absolutize(v) for v in node]
    return node


def check(method: str, path: str, status: int, body) -> None:
    schema = _absolutize(response_schema(method, path, status))
    Draft202012Validator(schema, registry=REGISTRY).validate(body)


@pytest.fixture
def client():
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://t")


class QuoteGateway:
    async def eta(self, a, b):
        return TripEstimate(10.0, 30.0, "dispatch-go")

    async def cell_load(self, lat, lng):
        return CellLoad("c9", supply=5, demand=10, source="dispatch-go")


def test_spec_documents_every_agents_route():
    """Every public agents-py route is in the gateway's OpenAPI document."""
    documented = {(m, p) for p, item in SPEC["paths"].items() for m in item}
    skip = {"/healthz", "/metrics", "/openapi.json", "/docs", "/docs/oauth2-redirect", "/redoc"}
    for route in app.routes:
        path = getattr(route, "path", "")
        if path in skip or not path.startswith(("/v1/", "/healthz")):
            continue
        for method in getattr(route, "methods", set()) - {"HEAD", "OPTIONS"}:
            assert (method.lower(), path) in documented, f"{method} {path} is not in docs/openapi.json"
            op = SPEC["paths"][path][method.lower()]
            assert op.get("x-served-by") == "agents-py", f"{method} {path} should be marked x-served-by: agents-py"


async def test_agents_platform_endpoints_match_spec(client):
    for path in ("/v1/agents", "/v1/agents/events", "/v1/agents/llm/budget", "/v1/agents/ping",
                 "/v1/agents/booking/personas", "/v1/agents/settlement/findings"):
        r = await client.get(path)
        assert r.status_code == 200, path
        check("GET", path, 200, r.json())
    r = await client.post("/v1/agents/settlement/scan")
    check("POST", "/v1/agents/settlement/scan", 200, r.json())
    r = await client.post("/v1/agents/parse-intent", json={"query": "from Koramangala to Indiranagar for 2 people"})
    check("POST", "/v1/agents/parse-intent", 200, r.json())
    r = await client.get("/v1/agents/events?limit=0")
    assert r.status_code == 422
    check("GET", "/v1/agents/events", 422, r.json())


async def test_booking_endpoints_match_spec(client, monkeypatch):
    agent, _ = make_agent(llm=make_llm(enabled=False))
    monkeypatch.setattr(planner, "_agent", agent)
    r = await client.post("/v1/agents/booking/parse", json={"rider_id": "rider_priya", "text": GOLDEN_SENTENCE})
    assert r.status_code == 200
    plan = r.json()
    check("POST", "/v1/agents/booking/parse", 200, plan)
    if plan["type"] == "plan":
        r = await client.post("/v1/agents/booking/confirm", json={"plan_id": plan["plan_id"]})
        assert r.status_code == 200
        check("POST", "/v1/agents/booking/confirm", 200, r.json())
    r = await client.post("/v1/agents/booking/confirm", json={"plan_id": "pln_missing"})
    assert r.status_code == 404
    check("POST", "/v1/agents/booking/confirm", 404, r.json())


async def test_pricing_endpoints_match_spec(client, monkeypatch):
    monkeypatch.setattr(pricing_api, "gateway", lambda: QuoteGateway())
    r = await client.get("/v1/pricing/tiers")
    check("GET", "/v1/pricing/tiers", 200, r.json())
    r = await client.post("/v1/pricing/quote", json={"tier": "sedan", "pickup": {"lat": 12.97, "lng": 77.59},
                                                     "dropoff": {"lat": 13.0, "lng": 77.6}})
    assert r.status_code == 200
    check("POST", "/v1/pricing/quote", 200, r.json())
    r = await client.post("/v1/pricing/quote", json={"tier": "mini"})
    assert r.status_code == 422
    check("POST", "/v1/pricing/quote", 422, r.json())


async def test_safety_endpoints_match_spec(client):
    signal = {"signal_id": "sig_c", "ride_id": "ride_c", "driver_id": "d_c", "severity": 0.95,
              "triggers": [{"type": "harsh_deceleration", "value": 1.4, "severity": 0.9}],
              "location": {"lat": 12.97, "lng": 77.59}}
    r = await client.post("/v1/safety/signal", json=signal)
    assert r.status_code == 202
    decision = r.json()
    check("POST", "/v1/safety/signal", 202, decision)
    r = await client.get("/v1/safety/incidents")
    check("GET", "/v1/safety/incidents", 200, r.json())
    r = await client.get("/v1/safety/checkins?status=all")
    check("GET", "/v1/safety/checkins", 200, r.json())
    if decision.get("incident_id"):
        r = await client.get(f"/v1/safety/incidents/{decision['incident_id']}")
        assert r.status_code == 200
        check("GET", "/v1/safety/incidents/{incident_id}", 200, r.json())
    if decision.get("checkin_id"):
        r = await client.get(f"/v1/safety/checkins/{decision['checkin_id']}")
        check("GET", "/v1/safety/checkins/{checkin_id}", 200, r.json())
    r = await client.get("/v1/safety/incidents/inc_missing")
    assert r.status_code == 404
    check("GET", "/v1/safety/incidents/{incident_id}", 404, r.json())


def test_validator_rejects_a_bad_body():
    with pytest.raises(Exception):
        check("GET", "/v1/pricing/tiers", 200, {"currency": "INR", "tiers": {"mini": {"base": "free"}}})
    with pytest.raises(Exception):
        check("GET", "/v1/agents", 200, {"agents": [{"id": "x"}], "total": 1})
