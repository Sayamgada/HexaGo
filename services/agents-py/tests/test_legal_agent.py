"""Legal & Jurisdiction agent: LLM ingest with the regex fallback, auto-apply + audit + agent event,
the 30 s cached surge cap hook, the API, and pricing reading the cap."""
import datetime as dt

import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents.legal import legal_agent as la
from src.agents_py.agents.legal.legal_store import LegalStore
from src.agents_py.agents.pricing import surge_map
from src.agents_py.config import settings
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore
from src.agents_py.main import app
from tests.helpers import FakeClock, ScriptedProvider, llm_response, make_llm
from tests.test_openapi_contract import check
from tests.test_legal_rules import SAMPLE_NOTICE

NOW = dt.datetime(2026, 11, 20, 6, 0, tzinfo=dt.timezone.utc)


def make(script=None, **llm_kw):
    events = EventStore(clock=lambda: NOW)
    llm = make_llm(completion=ScriptedProvider(script or []), fallback_models="", **llm_kw)
    agent = la.LegalAgent(store=LegalStore(clock=lambda: NOW), llm=llm, events=events)
    return agent, events


@pytest.fixture(autouse=True)
def _fresh_agent():
    la.set_agent(None)
    la.invalidate_caps()
    yield
    la.set_agent(None)
    la.invalidate_caps()


async def test_ingest_with_llm_applies_diff_audits_and_logs_event():
    agent, events = make([llm_response({"max_surge_multiplier": 1.5, "summary": "Surge capped at 1.5x."})])
    out = await agent.ingest(SAMPLE_NOTICE)
    assert out["city"] == "bengaluru"  # inferred from "Karnataka"
    assert out["applied"] is True and out["source"] == "llm" and out["used_fallback"] is False
    assert out["diff"] == [{"field": "max_surge_multiplier", "old": 2.5, "new": 1.5}]
    assert out["jurisdiction"]["rules"]["max_surge_multiplier"] == 1.5 and out["jurisdiction"]["version"] == 2
    assert out["summary"] == "Surge capped at 1.5x."
    audit = await agent.store.audit()
    assert audit[0]["id"] == out["audit_id"] and audit[0]["diff"] == out["diff"] and audit[0]["text"] == SAMPLE_NOTICE
    ev = (await events.recent())[0]
    assert ev["agent"] == "legal_jurisdiction" and ev["used_fallback"] is False
    assert ev["decision"] == "Bengaluru: max_surge_multiplier 2.5 -> 1.5 applied"
    assert ev["metadata"]["kind"] == "legal_rule_change" and ev["metadata"]["audit_id"] == out["audit_id"]


async def test_ingest_falls_back_to_rules_when_llm_unavailable():
    agent, events = make(enabled=False)
    out = await agent.ingest("Maharashtra notice: a surge cap of 1.8x and a maximum 10 driving hours per day.",
                             city="Bombay")
    assert out["city"] == "mumbai" and out["source"] == "rules"
    assert out["used_fallback"] is True and out["fallback_reason"] == "disabled"
    assert {d["field"]: d["new"] for d in out["diff"]} == {"max_surge_multiplier": 1.8, "max_driving_hours": 10}
    ev = (await events.recent())[0]
    assert ev["used_fallback"] is True and ev["metadata"]["model"] == "rule_extractor"


async def test_text_numbers_override_a_disagreeing_llm_and_bad_values_are_rejected():
    agent, _ = make([llm_response({"max_surge_multiplier": 1.2, "city_levy_pct": 55})])
    out = await agent.ingest(SAMPLE_NOTICE, city="bengaluru")
    assert out["overridden"] == ["max_surge_multiplier"]
    assert out["diff"] == [{"field": "max_surge_multiplier", "old": 2.5, "new": 1.5}]
    assert out["rejected"] == [{"field": "city_levy_pct", "value": 55, "reason": "outside 0..20"}]


async def test_ingest_without_changes_is_audited_but_not_applied():
    agent, events = make(enabled=False)
    out = await agent.ingest("Season's greetings from the transport department.")
    assert out["city"] == "bengaluru" and out["applied"] is False and out["diff"] == []
    assert out["jurisdiction"]["version"] == 1
    assert len(await agent.store.audit()) == 1
    ev = (await events.recent())[0]
    assert ev["decision"] == "Bengaluru: no rule changes in the text" and ev["metadata"]["kind"] == "legal_no_change"


async def test_ingest_unknown_city_raises():
    agent, _ = make(enabled=False)
    with pytest.raises(la.UnknownCity):
        await agent.ingest(SAMPLE_NOTICE, city="Paris")


async def test_surge_cap_hook_is_cached_for_30_s_and_invalidated_on_apply():
    agent, _ = make(enabled=False)
    la.set_agent(agent)
    clock = FakeClock(1000.0)
    assert await la.get_surge_cap("Bengaluru", clock=clock) == 2.5
    # a change made behind the cache (another instance) shows only after the TTL
    j = await agent.store.get("bengaluru")
    await agent.store.update_rules("bengaluru", {**j["rules"], "max_surge_multiplier": 2.0}, by="other")
    clock.advance(29)
    assert await la.get_surge_cap("bengaluru", clock=clock) == 2.5
    clock.advance(2)
    assert await la.get_surge_cap("bengaluru", clock=clock) == 2.0
    # a local ingest invalidates the cache at once
    await agent.ingest(SAMPLE_NOTICE)
    assert await la.get_surge_cap("bengaluru", clock=clock) == 1.5
    assert await la.get_surge_cap("Paris", default=2.5, clock=clock) == 2.5
    assert await la.get_surge_cap(None, clock=clock) == 1.5  # settings.CITY_NAME


async def test_surge_cap_hook_disabled_and_store_failure(monkeypatch):
    agent, _ = make(enabled=False)
    la.set_agent(agent)
    monkeypatch.setattr(settings, "LEGAL_ENABLED", False)
    assert await la.get_surge_cap("bengaluru", default=3.0) == 3.0
    monkeypatch.setattr(settings, "LEGAL_ENABLED", True)
    clock = FakeClock(0.0)
    assert await la.get_surge_cap("delhi", clock=clock) == 2.0

    async def boom(city):
        raise RuntimeError("db down")

    monkeypatch.setattr(agent.store, "get", boom)
    clock.advance(60)
    assert await la.get_surge_cap("delhi", clock=clock) == 2.0  # last known value
    assert await la.get_surge_cap("mumbai", default=2.5, clock=clock) == 2.5


async def test_get_agent_singleton_and_registration():
    a = await la.get_agent()
    assert a is await la.get_agent() and a.store.backend == "memory"
    assert "legal_jurisdiction" in registry.registered()


# ---------------------------------------------------------------- API

@pytest.fixture
def client():
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://t")


async def test_api_ingest_jurisdictions_and_audit(client, monkeypatch):
    monkeypatch.setattr(settings, "LLM_ENABLED", False)
    async with client as c:
        r = await c.get("/v1/legal/jurisdictions")
        assert r.status_code == 200
        body = r.json()
        assert body["enabled"] is True and [j["city"] for j in body["jurisdictions"]] == ["bengaluru", "delhi", "mumbai"]
        assert body["sample_notice"] == la.SAMPLE_NOTICE
        check("GET", "/v1/legal/jurisdictions", 200, body)

        r = await c.post("/v1/legal/ingest", json={"city": "Bengaluru", "text": SAMPLE_NOTICE})
        assert r.status_code == 200
        out = r.json()
        assert out["applied"] is True and out["diff"][0]["new"] == 1.5
        check("POST", "/v1/legal/ingest", 200, out)

        one = (await c.get("/v1/legal/jurisdictions/bangalore")).json()
        assert one["rules"]["max_surge_multiplier"] == 1.5
        check("GET", "/v1/legal/jurisdictions/{city}", 200, one)
        assert (await c.get("/v1/legal/jurisdictions/paris")).status_code == 404

        audit = (await c.get("/v1/legal/audit", params={"city": "bengaluru", "limit": 5})).json()
        assert audit["entries"][0]["id"] == out["audit_id"] and audit["count"] == 1
        check("GET", "/v1/legal/audit", 200, audit)

        assert (await c.post("/v1/legal/ingest", json={"city": "Paris", "text": "x"})).status_code == 422
        assert (await c.post("/v1/legal/ingest", json={"text": ""})).status_code == 422


async def test_api_disabled(client, monkeypatch):
    monkeypatch.setattr(settings, "LEGAL_ENABLED", False)
    async with client as c:
        assert (await c.post("/v1/legal/ingest", json={"text": SAMPLE_NOTICE})).status_code == 503
        assert (await c.get("/v1/legal/jurisdictions")).json()["enabled"] is False


# ---------------------------------------------------------------- pricing reads the cap

class Gateway:
    def __init__(self, snap):
        self.snap = snap

    async def snapshot(self, window_s):
        return self.snap


def hot_snapshot():
    # 40 requests and no idle car in one spot: far above any cap
    return {"drivers": [], "requests": [{"lat": 12.9716, "lng": 77.5946}] * 40}


async def test_surge_map_and_quote_respect_the_legal_cap(client, monkeypatch):
    agent, _ = make(enabled=False)
    la.set_agent(agent)
    m = surge_map.SurgeMap(gateway=Gateway(hot_snapshot()), store=EventStore(), llm=make_llm(enabled=False))
    m.baseline = {"x": 1.0}  # not the first look: no self-seeding of today's pressure
    m.runs = 1
    before = await m.refresh()
    assert before["max_multiplier"] == 2.5
    await agent.ingest(SAMPLE_NOTICE)
    after = await m.refresh()
    assert after["max_multiplier"] == 1.5
    assert all(c["multiplier"] <= 1.5 for c in after["cells"])

    surge_map.set_map(m)
    async with client as c:
        q = (await c.post("/v1/pricing/quote", json={"tier": "mini", "distance_km": 5, "duration_min": 15,
                                                      "supply": 1, "demand": 50})).json()
        assert q["surge_multiplier"] == 1.5
        assert (await c.get("/v1/pricing/tiers")).json()["surge_cap"] == 1.5
