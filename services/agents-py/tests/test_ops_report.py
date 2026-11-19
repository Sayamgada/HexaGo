"""Ops Supervisor Autonomous Ops Report: collection, anomalies -> auto-actions, async LLM summary,
history + Postgres, the 5-minute loop and the API."""
import asyncio
import datetime as dt
import json

import httpx
import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.agents.supervisor import ops_report as orp
from src.agents_py.config import settings
from src.agents_py.core import registry
from src.agents_py.core.events import EventStore
from src.agents_py.core.registry import RegisteredAgent
from src.agents_py.main import app
from tests.helpers import FakePool, ScriptedProvider, llm_response, make_llm
from tests.test_openapi_contract import check
from tests.test_report import LEDGER, METRICS

NOW = dt.datetime(2026, 11, 20, 6, 0, tzinfo=dt.timezone.utc)


class Clock:
    def __init__(self, t=NOW):
        self.t = t

    def __call__(self):
        return self.t


class FakeDispatch:
    def __init__(self, metrics=None, ledger=None, fail=False):
        self.metrics = metrics if metrics is not None else METRICS
        self.ledger = ledger if ledger is not None else LEDGER
        self.fail = fail

    def handler(self, req: httpx.Request) -> httpx.Response:
        if self.fail:
            return httpx.Response(503, text="down")
        if req.url.path == "/v1/metrics/dispatch":
            return httpx.Response(200, json=self.metrics)
        if req.url.path == "/v1/ledger/summary":
            return httpx.Response(200, json=self.ledger)
        return httpx.Response(404)

    def sources(self) -> orp.DispatchSources:
        return orp.DispatchSources("http://dispatch", transport=httpx.MockTransport(self.handler))


def make(dispatch=None, incidents=None, script=None, pool=None, clock=None, **llm_kw):
    clock = clock or Clock()
    store = EventStore(clock=clock)
    llm_kw.setdefault("fallback_models", "")
    llm = make_llm(completion=ScriptedProvider(script or []), **llm_kw)

    async def list_incidents():
        if incidents == "fail":
            raise RuntimeError("sentinel down")
        return list(incidents or [])

    rep = orp.OpsReporter(sources=(dispatch or FakeDispatch()).sources(), store=store, llm=llm, pool=pool,
                          incidents=list_incidents, clock=clock)
    return rep, store


@pytest.fixture(autouse=True)
def _fresh():
    orp.set_reporter(None)
    yield
    orp.set_reporter(None)


async def test_run_builds_report_records_event_and_keeps_history():
    rep, store = make(enabled=False)
    out = await rep.run("manual")
    assert out["id"].startswith("opr_20261120T060000") and out["trigger"] == "manual"
    assert out["human_interventions"] == 0 and out["anomalies"] == []
    assert out["kpis"]["matches_today"] == 540 and out["money"]["trial_balance_paise"] == 0
    assert out["sources"] == {"agents": "ok", "metrics": "ok", "ledger": "ok", "incidents": "ok", "llm": "ok"}
    assert "## Human interventions: 0" in out["markdown"]
    ev = (await store.recent())[0]
    assert ev["agent"] == "ops_supervisor" and ev["metadata"]["kind"] == "ops_report"
    assert ev["decision"] == f"ops report {out['id']}: 0 anomalies, 0 human interventions"
    assert rep.latest()["id"] == out["id"]
    second = await rep.run("schedule")
    hist = await rep.history()
    assert [h["id"] for h in hist] == [second["id"], out["id"]]
    assert set(hist[0]) == {"id", "generated_at", "trigger", "anomalies", "summary_source", "human_interventions",
                            "headline"}
    assert (await rep.get(out["id"]))["id"] == out["id"] and await rep.get("nope") is None


async def test_unavailable_sources_are_reported_not_raised():
    rep, _ = make(dispatch=FakeDispatch(fail=True), incidents="fail", enabled=False)
    out = await rep.run()
    assert out["sources"]["metrics"].startswith("unavailable") and out["sources"]["ledger"].startswith("unavailable")
    assert out["sources"]["incidents"] == "unavailable: RuntimeError: sentinel down"
    assert out["money"] is None and "Ledger summary unavailable." in out["markdown"]


async def test_anomalies_trigger_auto_actions_and_events_once(monkeypatch):
    calls = []

    async def settlement_scan():
        calls.append("settlement")
        return {}

    async def needs_args(signal):
        return {}

    monkeypatch.setitem(registry._registered, "settlement",
                        RegisteredAgent("settlement", "Settlement", "", settlement_scan))
    monkeypatch.setitem(registry._registered, "safety_sentinel",
                        RegisteredAgent("safety_sentinel", "Safety Sentinel", "", needs_args))
    clock = Clock(NOW - dt.timedelta(minutes=12))
    incidents = [{"id": "inc_old", "status": "escalated", "driver_id": "d_8",
                  "created_at": "2026-11-20T05:30:00Z"}]
    rep, store = make(dispatch=FakeDispatch(ledger={**LEDGER, "trial_balance_paise": 100, "balanced": False}),
                      incidents=incidents, clock=clock, enabled=False)
    for _ in range(6):  # settlement busy 12 min ago, booking intent on fallbacks
        await store.record_event("settlement", "scan", "ok", "", 1.0, 1, False)
    clock.t = NOW - dt.timedelta(minutes=1)
    for _ in range(6):
        await store.record_event("booking_intent", "parse", "plan", "", 0.8, 1, True, "rate")
    clock.t = NOW

    out = await rep.run()
    kinds = [(a["kind"], a["subject"]) for a in out["anomalies"]]
    assert kinds == [("high_fallback_rate", "booking_intent"), ("silent_agent", "settlement"),
                     ("stale_incident", "inc_old"), ("ledger_imbalance", "ledger")]
    acts = {(a["kind"], a["subject"]): a for a in out["actions"]}
    assert acts[("silent_agent", "settlement")] == {"kind": "silent_agent", "subject": "settlement",
                                                    "action": "re-ran Settlement", "result": "ok"}
    assert acts[("ledger_imbalance", "ledger")]["action"] == "triggered a Settlement ledger audit"
    assert acts[("stale_incident", "inc_old")]["result"] == "pending_review"
    assert acts[("high_fallback_rate", "booking_intent")]["result"] == "ok"
    assert calls == ["settlement", "settlement"]
    alerts = [e for e in await store.recent(limit=100) if e["metadata"].get("kind") == "ops_anomaly"]
    assert len(alerts) == 4
    assert "→ re-ran Settlement (ok)" in out["markdown"]

    await rep.run()  # same anomalies again: actions run, but no duplicate alert events
    alerts = [e for e in await store.recent(limit=100) if e["metadata"].get("kind") == "ops_anomaly"]
    assert len(alerts) == 4


async def test_silent_agent_without_runnable_handler_and_failing_handler(monkeypatch):
    async def needs_args(x):
        return x

    async def broken():
        raise RuntimeError("boom")

    rep, _ = make(enabled=False)
    monkeypatch.setitem(registry._registered, "dispatch_broker",
                        RegisteredAgent("dispatch_broker", "Dispatch Broker", "", needs_args))
    act = await rep.act({"kind": "silent_agent", "subject": "dispatch_broker"})
    assert act["action"] == "alerted the Ops feed (no zero-argument handler to re-run)"
    monkeypatch.setitem(registry._registered, "settlement", RegisteredAgent("settlement", "Settlement", "", broken))
    act = await rep.act({"kind": "silent_agent", "subject": "settlement"})
    assert act["result"] == "failed: RuntimeError: boom"
    act = await rep.act({"kind": "silent_agent", "subject": "demand_forecast_x"})
    assert act["action"].startswith("alerted the Ops feed")
    monkeypatch.delitem(registry._registered, "settlement")
    act = await rep.act({"kind": "ledger_imbalance", "subject": "ledger"})
    assert act["action"] == "alerted the Ops feed (Settlement agent not running here)"


async def test_llm_writes_the_summary_asynchronously():
    rep, store = make(script=[llm_response({"summary": "The swarm ran itself all morning."})])
    out = await rep.run()
    assert out["summary_source"] == "template"
    await rep.idle()
    latest = rep.latest()
    assert latest["summary"] == "The swarm ran itself all morning." and latest["summary_source"] == "llm"
    assert "The swarm ran itself all morning." in latest["markdown"] and "_Summary written by the LLM" in latest["markdown"]
    ev = (await store.recent())[0]
    assert ev["metadata"]["kind"] == "ops_report_summary"


async def test_llm_failure_keeps_the_template():
    rep, _ = make(script=[llm_response("not json")])
    out = await rep.run()
    await rep.idle()
    assert rep.latest()["summary_source"] == "template" and rep.latest()["summary"] == out["summary"]


async def test_postgres_persistence():
    saved = {}

    def handler(sql, args):
        if "to_regclass" in sql:
            return None
        if "INSERT INTO ops_reports" in sql:
            saved[args[0]] = args[2]
        if "SELECT report FROM ops_reports WHERE id" in sql:
            return {"report": saved[args[0]]} if args[0] in saved else None
        if "SELECT report FROM ops_reports ORDER BY" in sql:
            return [{"report": v} for v in reversed(list(saved.values()))]
        return None

    pool = FakePool(handler)
    rep, _ = make(pool=pool, enabled=False)
    out = await rep.run()
    assert "CREATE TABLE IF NOT EXISTS ops_reports" in " ".join(pool.sql())
    assert json.loads(saved[out["id"]])["id"] == out["id"]
    fresh, _ = make(pool=FakePool(handler), enabled=False)
    assert (await fresh.get(out["id"]))["id"] == out["id"]
    assert (await fresh.history())[0]["id"] == out["id"]
    assert (await fresh.latest_or_stored())["id"] == out["id"]
    broken, _ = make(pool=FakePool(fail=True), enabled=False)
    assert (await broken.run())["id"]  # a DB failure never breaks the report
    assert await broken.history() and await broken.get("nope") is None


async def test_loop_runs_then_sleeps():
    rep, _ = make(enabled=False)
    waits = []

    async def sleep(s):
        waits.append(s)
        if len(waits) == 2:
            raise asyncio.CancelledError

    with pytest.raises(asyncio.CancelledError):
        await orp.report_loop(300, rep, sleep=sleep)
    assert waits == [300, 300] and len(await rep.history()) == 2
    assert (await rep.history())[0]["trigger"] == "schedule"

    class Broken(orp.OpsReporter):
        async def run(self, trigger="manual"):
            raise RuntimeError("x")

    waits.clear()
    with pytest.raises(asyncio.CancelledError):
        await orp.report_loop(5, Broken(), sleep=sleep)  # survives a failed run
    assert waits == [5, 5]


async def test_get_reporter_singleton():
    r = await orp.get_reporter()
    assert r is await orp.get_reporter()


# ---------------------------------------------------------------- API

@pytest.fixture
def client():
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://t")


async def test_api(client, monkeypatch):
    rep, _ = make(enabled=False)
    orp.set_reporter(rep)
    async with client as c:
        first = (await c.get("/v1/ops/report")).json()  # none yet: generated on demand
        assert first["trigger"] == "on_demand" and first["markdown"].startswith("# Autonomous Ops Report")
        check("GET", "/v1/ops/report", 200, first)
        assert (await c.get("/v1/ops/report")).json()["id"] == first["id"]
        run = await c.post("/v1/ops/report/run")
        assert run.status_code == 200 and run.json()["trigger"] == "manual"
        hist = (await c.get("/v1/ops/reports", params={"limit": 5})).json()
        assert hist["count"] == 2 and hist["reports"][0]["id"] == run.json()["id"]
        check("POST", "/v1/ops/report/run", 200, run.json())
        check("GET", "/v1/ops/reports", 200, hist)
        assert (await c.get(f"/v1/ops/reports/{first['id']}")).json()["id"] == first["id"]
        assert (await c.get("/v1/ops/reports/opr_nope")).status_code == 404
        monkeypatch.setattr(settings, "OPS_REPORT_ENABLED", False)
        assert (await c.post("/v1/ops/report/run")).status_code == 503
        assert (await c.get("/v1/ops/report")).status_code == 503


async def test_default_sources_lazy_clients_and_sentinel_incidents(monkeypatch):
    from src.agents_py.agents.safety import sentinel as sentinel_mod

    class FakeSentinel:
        class store:
            @staticmethod
            async def list_incidents(limit):
                return [{"id": "inc_1"}]

        @staticmethod
        def summary(i):
            return {**i, "status": "monitoring"}

    async def fake_get_sentinel():
        return FakeSentinel

    monkeypatch.setattr(sentinel_mod, "get_sentinel", fake_get_sentinel)
    assert await orp._sentinel_incidents() == [{"id": "inc_1", "status": "monitoring"}]
    r = orp.OpsReporter()
    assert r.sources.base_url == settings.DISPATCH_URL.rstrip("/")
    assert (await r.store()) is (await r.store()) and (await r.llm()) is (await r.llm())
    assert orp._loads(b"not json") == {} and orp._loads({"a": 1}) == {"a": 1} and orp._loads(None) == {}
    assert orp._runnable(lambda x=1: x) is True and orp._runnable(lambda x: x) is False


async def test_summary_exception_is_swallowed():
    rep, _ = make(enabled=False)
    out = await rep.run()

    class Boom:
        async def complete_json(self, *a, **k):
            raise RuntimeError("x")

    rep._llm = Boom()
    await rep._summarize(out)  # logged, not raised
    assert rep.latest()["summary_source"] == "template"


async def test_get_reporter_refreshes_pool(monkeypatch):
    from src.agents_py.core import infra

    r = await orp.get_reporter()
    pool = FakePool()
    monkeypatch.setattr(infra, "_pool", pool)
    assert (await orp.get_reporter()).pool is pool and r.pool is pool
