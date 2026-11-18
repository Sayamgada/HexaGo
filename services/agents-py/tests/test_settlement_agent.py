"""Settlement agent: periodic deterministic scan, async LLM explanations, audit trail, registry, API."""
import asyncio
import datetime as dt

import httpx
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents.settlement import agent as sa
from src.agents_py.agents.settlement import api as settlement_api  # noqa: F401 - registers the agent
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore
from src.agents_py.main import app
from tests.helpers import ScriptedProvider, llm_response, make_llm
from tests.test_settlement_checks import NOW, ride_entry, ts


class FakeLedger:
    """dispatch-go's /v1/ledger/entries and /v1/ledger/payouts behind an httpx MockTransport."""

    def __init__(self, entries=None, payouts=None, fail=False):
        self.entries = list(entries or [])
        self.payouts = list(payouts or [])
        self.fail = fail
        self.requests = []

    def handler(self, req: httpx.Request) -> httpx.Response:
        self.requests.append(req)
        if self.fail:
            return httpx.Response(503, text="down")
        if req.url.path == "/v1/ledger/entries":
            return httpx.Response(200, json={"entries": self.entries})
        status = req.url.params.get("status")
        return httpx.Response(200, json={"payouts": [p for p in self.payouts if not status or p["status"] == status]})

    def client(self) -> sa.LedgerClient:
        return sa.LedgerClient("http://dispatch", transport=httpx.MockTransport(self.handler))


def make_agent(ledger, llm=None, store=None):
    store = store or EventStore(clock=lambda: NOW)
    llm = llm or make_llm(fallback_models="")
    return sa.SettlementAgent(client=ledger.client(), store=store, llm=llm, clock=lambda: NOW), store


ANOMALIES = [ride_entry("tipbig", tip=60_000), ride_entry("bad", tamper={"platform_fee": -100, "card": 100})]
PAYOUTS = [{"id": "po_slow", "ride_id": "slow", "driver_id": "d_2", "status": "sent", "created_at": ts(60),
            "sent_at": ts(50)},
           {"id": "po_held", "ride_id": "held", "driver_id": "d_3", "status": "held_for_review", "created_at": ts(3600),
            "held_at": ts(3600), "incident_id": "inc_9", "amount_paise": 9258}]


async def test_clean_scan_logs_run():
    ledger = FakeLedger([ride_entry("a"), ride_entry("b")], [{"id": "po_a", "ride_id": "a", "status": "sent",
                                                               "created_at": ts(5), "sent_at": ts(4.7)}])
    agent, store = make_agent(ledger)
    run = await agent.scan()
    assert run == {**run, "ok": True, "entries": 2, "rides": 2, "payouts": 1, "flagged": 0, "new": 0}
    ev = (await store.recent(5))[0]
    assert ev["agent"] == "settlement" and ev["decision"] == "ledger scan: no anomalies" and not ev["used_fallback"]
    assert ev["metadata"]["kind"] == "settlement_scan" and ev["metadata"]["findings"] == []
    # It asked for recent entries/payouts and for held and pending payouts of any age.
    paths = [(r.url.path, r.url.params.get("status")) for r in ledger.requests]
    assert paths == [("/v1/ledger/entries", None), ("/v1/ledger/payouts", None),
                     ("/v1/ledger/payouts", "held_for_review"), ("/v1/ledger/payouts", "pending")]
    assert ledger.requests[0].url.params["since"] == "2026-11-18T09:45:00Z"


async def test_flagged_items_get_template_then_async_llm_explanation():
    gate = asyncio.Event()

    async def explained(**_):
        await gate.wait()
        return llm_response({"explanation": "A manual posting skewed the platform fee.", "recommended_action": "audit"})

    llm = make_llm(completion=ScriptedProvider([explained]), fallback_models="", timeout_s=2)
    ledger = FakeLedger(ANOMALIES, PAYOUTS)
    agent, store = make_agent(ledger, llm)
    run = await agent.scan()
    assert run["flagged"] == 4 and run["new"] == 4
    assert run["by_check"] == {"split_mismatch": 1, "unusual_tip": 1, "payout_delayed": 1, "held_payout_stale": 1}
    # Before the LLM answers, every finding carries its deterministic template.
    views = agent.view()["findings"]
    assert all(v["explanation_source"] == "template" and v["explanation"] for v in views)
    scan_ev = (await store.recent(1))[0]
    assert scan_ev["decision"] == "ledger scan: 4 anomalies flagged" and len(scan_ev["metadata"]["findings"]) == 4

    gate.set()
    await agent.idle()
    by_key = {v["key"]: v for v in agent.view()["findings"]}
    top = by_key["split_mismatch:je_bad"]  # most severe -> explained first, by the LLM
    assert top["explanation_source"] == "llm" and top["explanation"].startswith("A manual posting")
    assert top["recommended_action"] == "audit"
    # The per-agent LLM rate limit turns the next explanations into templates (fallback "rate").
    assert by_key["held_payout_stale:po_held"]["explanation_source"] == "template"
    anomaly_events = [e for e in await store.recent(20) if e["metadata"].get("kind") == "settlement_anomaly"]
    assert len(anomaly_events) == 3  # MAX_EXPLANATIONS per run
    assert sorted(e["used_fallback"] for e in anomaly_events) == [False, True, True]
    assert {e["fallback_reason"] for e in anomaly_events if e["used_fallback"]} == {"rate"}

    # A second scan reports the same findings but does not explain them again.
    run2 = await agent.scan()
    await agent.idle()
    assert run2["flagged"] == 4 and run2["new"] == 0
    assert len([e for e in await store.recent(50) if e["metadata"].get("kind") == "settlement_anomaly"]) == 3


async def test_llm_disabled_keeps_templates():
    agent, store = make_agent(FakeLedger([ride_entry("tipbig", tip=60_000)]), make_llm(enabled=False))
    await agent.scan()
    await agent.idle()
    v = agent.view()["findings"][0]
    assert v["explanation_source"] == "template" and "unusually large tip" in v["explanation"]
    ev = [e for e in await store.recent(5) if e["metadata"].get("kind") == "settlement_anomaly"][0]
    assert ev["used_fallback"] and ev["fallback_reason"] == "disabled"


async def test_explanation_errors_never_break_the_agent():
    class Exploding:
        async def complete_json(self, *a, **k):
            raise RuntimeError("provider exploded")

    agent, store = make_agent(FakeLedger([ride_entry("tipbig", tip=60_000)]), Exploding())
    await agent.scan()
    await agent.idle()
    assert agent.view()["findings"][0]["explanation_source"] == "template"
    # A finding evicted before its explanation lands is simply skipped.
    agent2, _ = make_agent(FakeLedger([ride_entry("tipbig", tip=60_000)]), make_llm(fallback_models=""))
    await agent2.scan()
    agent2.findings.clear()
    await agent2.idle()
    assert agent2.view()["findings"] == []


async def test_unreachable_ledger_is_logged_as_fallback():
    agent, store = make_agent(FakeLedger(fail=True))
    run = await agent.scan()
    assert run["ok"] is False and "503" in run["error"]
    ev = (await store.recent(1))[0]
    assert ev["used_fallback"] and ev["fallback_reason"] == "error" and "unreachable" in ev["decision"]
    transport_err = sa.LedgerClient("http://dispatch", transport=httpx.MockTransport(
        lambda r: (_ for _ in ()).throw(httpx.ConnectError("refused"))))
    agent2 = sa.SettlementAgent(client=transport_err, store=store, llm=make_llm(), clock=lambda: NOW)
    assert (await agent2.scan())["ok"] is False
    await transport_err.aclose()


async def test_findings_are_bounded(monkeypatch):
    monkeypatch.setattr(sa, "MAX_FINDINGS", 2)
    agent, _ = make_agent(FakeLedger(ANOMALIES, PAYOUTS), make_llm(enabled=False))
    await agent.scan()
    await agent.idle()
    assert len(agent.findings) == 2


async def test_scan_loop_survives_failures():
    calls = []

    class Flaky:
        async def scan(self):
            calls.append(1)
            if len(calls) == 1:
                raise RuntimeError("boom")
            return {}

    async def sleep(_):
        if len(calls) >= 3:
            raise asyncio.CancelledError

    try:
        await sa.scan_loop(60, Flaky(), sleep=sleep)
    except asyncio.CancelledError:
        pass
    assert len(calls) == 3


async def test_registry_marks_settlement_active_and_endpoints(monkeypatch):
    assert registry.status_of("settlement") == "active"
    ledger = FakeLedger([ride_entry("tipbig", tip=60_000)])
    agent, _ = make_agent(ledger, make_llm(enabled=False))
    sa.set_agent(agent)
    try:
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as client:
            r = await client.post("/v1/agents/settlement/scan")
            assert r.status_code == 200 and r.json()["last_run"]["flagged"] == 1
            r = await client.get("/v1/agents/settlement/findings")
            body = r.json()
            assert body["runs"] == 1 and body["findings"][0]["check"] == "unusual_tip"
            r = await client.get("/v1/agents")
            st = {a["id"]: a for a in r.json()["agents"]}["settlement"]
            assert st["status"] == "active" and st["runs_in"] == "agents-py"
    finally:
        await agent.idle()
        sa.set_agent(None)
    assert isinstance(sa.get_agent(), sa.SettlementAgent)
    sa.set_agent(None)


async def test_default_store_and_llm_are_lazily_resolved():
    agent = sa.SettlementAgent(client=FakeLedger().client(), clock=lambda: NOW)
    assert (await agent.scan())["ok"] is True
    assert agent._store is not None and (await agent.llm()) is not None
    assert dt.datetime.now(dt.timezone.utc) - sa.SettlementAgent(client=FakeLedger().client()).clock() < dt.timedelta(seconds=5)
