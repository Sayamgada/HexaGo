"""Ops Supervisor report builder: anomaly rules, KPIs, deterministic markdown."""
import datetime as dt

from src.agents_py.agents.supervisor import report as rp

NOW = dt.datetime(2026, 11, 20, 6, 0, tzinfo=dt.timezone.utc)


def ts(minutes_ago: float) -> str:
    return (NOW - dt.timedelta(minutes=minutes_ago)).strftime("%Y-%m-%dT%H:%M:%SZ")


def agent_row(aid, decisions=0, rate=0.0, last_min=None, status="active", name=None):
    return {"id": aid, "name": name or aid.replace("_", " ").title(), "status": status,
            "decisions_last_hour": decisions, "fallback_rate": rate,
            "last_decision": None if last_min is None else {"ts": ts(last_min), "decision": "x"}}


METRICS = {"p99_ms": 0.75, "matches_today": 540, "rides_settled_today": 146, "decisions_today": 668,
           "rides_in_progress": 174, "active_drivers": 300, "idle_drivers": 130, "open_requests": 2}
LEDGER = {"trial_balance_paise": 0, "balanced": True, "platform_revenue_paise": 4796800, "rides_settled_today": 146,
          "payouts_today": {"sent_count": 145, "sent_paise": 1644760, "held_count": 3, "held_paise": 31840}}
BUDGET = {"calls_today": 12, "tokens_today": 3400, "limits": {"daily_calls": 300, "daily_tokens": 200000},
          "enabled": True}


def inputs(**kw):
    base = dict(
        now=NOW,
        agents=[agent_row("dispatch_broker", 120, 0.0, 0.2), agent_row("settlement", 60, 0.0, 0.5),
                agent_row("ops_supervisor", 12, 0.0, 1), agent_row("booking_intent", 2, 0.5, 30),
                agent_row("pool_batching", status="planned")],
        metrics=METRICS, ledger=LEDGER,
        incidents=[{"id": "inc_1", "status": "monitoring", "decision": "monitor", "driver_id": "d_1",
                    "created_at": ts(40)}],
        budget=BUDGET, sources={"metrics": "ok", "ledger": "ok", "incidents": "ok", "agents": "ok", "llm": "ok"},
    )
    base.update(kw)
    return rp.ReportInputs(**base)


def test_healthy_swarm_has_no_anomalies():
    assert rp.detect_anomalies(inputs()) == []


def test_high_fallback_rate_needs_enough_decisions():
    inp = inputs(agents=[agent_row("booking_intent", 10, 0.8, 1), agent_row("surge_equilibrium", 4, 1.0, 1),
                         agent_row("settlement", 10, 0.5, 1)])
    got = rp.detect_anomalies(inp)
    assert [(a["kind"], a["subject"]) for a in got] == [("high_fallback_rate", "booking_intent")]
    assert got[0]["detail"] == "fallback rate 80% over 10 decisions in the last hour (limit 50%)"
    assert got[0]["severity"] == "warning"


def test_silent_active_agent_but_not_event_driven_or_planned():
    inp = inputs(agents=[agent_row("settlement", 30, 0.0, 12), agent_row("dispatch_broker", 50, 0.0, 9.5),
                         agent_row("safety_sentinel", 3, 0.0, 50), agent_row("legal_jurisdiction", 1, 0.0, 40),
                         agent_row("compliance_vision", 0, status="planned"), agent_row("demand_forecast", 0)])
    got = rp.detect_anomalies(inp)
    assert [(a["kind"], a["subject"]) for a in got] == [("silent_agent", "settlement")]
    assert got[0]["detail"] == "no events for 12 min (last at 05:48 UTC); active in the last hour"


def test_stale_open_incidents_and_ledger_imbalance():
    inc = [{"id": "inc_old", "status": "escalated", "driver_id": "d_8", "created_at": ts(20)},
           {"id": "inc_new", "status": "check_in", "driver_id": "d_9", "created_at": ts(5)},
           {"id": "inc_mon", "status": "monitoring", "driver_id": "d_7", "created_at": ts(90)},
           {"id": "inc_done", "status": "resolved", "driver_id": "d_6", "created_at": ts(90)},
           {"id": "inc_bad", "status": "escalated", "created_at": "garbage"}]
    got = rp.detect_anomalies(inputs(incidents=inc, ledger={**LEDGER, "trial_balance_paise": 100, "balanced": False}))
    assert [(a["kind"], a["subject"]) for a in got] == [("stale_incident", "inc_old"), ("ledger_imbalance", "ledger")]
    assert got[0]["severity"] == "critical" and "open 20 min" in got[0]["detail"]
    assert got[1]["detail"] == "trial balance is ₹1.00, not ₹0.00"


def test_missing_sources_do_not_raise():
    inp = inputs(metrics=None, ledger=None, budget=None, incidents=[],
                 sources={"metrics": "unavailable: down", "ledger": "unavailable: down"})
    assert rp.detect_anomalies(inp) == []
    rep = rp.build_report(inp, [], [], trigger="manual", report_id="opr_x")
    assert rep["money"] is None and rep["kpis"]["matches_today"] is None
    md = rep["markdown"]
    assert "Ledger summary unavailable" in md and "| Matches today | n/a |" in md
    assert "metrics unavailable: down" in md


def test_build_report_kpis_sections_and_markdown():
    inp = inputs(agents=[*inputs().agents, agent_row("booking_intent", 10, 0.8, 1, name="Booking Intent")])
    anomalies = rp.detect_anomalies(inp)
    actions = [{"kind": "high_fallback_rate", "subject": "booking_intent", "action": "alerted the Ops feed",
                "result": "ok"}]
    rep = rp.build_report(inp, anomalies, actions, trigger="schedule", report_id="opr_1")
    k = rep["kpis"]
    assert k["decisions_today"] == 668 and k["matches_today"] == 540 and k["llm_calls_today"] == 12
    assert k["decisions_last_hour"] == 204 and k["active_agents"] == 5 and k["total_agents"] == 6
    assert rep["human_interventions"] == 0 and rep["id"] == "opr_1" and rep["generated_at"] == "2026-11-20T06:00:00Z"
    assert rep["summary_source"] == "template" and rep["summary"] == rp.template_summary(rep)
    assert rep["money"]["trial_balance_paise"] == 0 and rep["money"]["payouts_sent_count"] == 145
    assert rep["incidents"] == {"total": 1, "open": 0, "escalated": 0, "stale": 0, "recent": [
        {"id": "inc_1", "status": "monitoring", "decision": "monitor", "driver_id": "d_1", "age_min": 40}]}

    md = rep["markdown"]
    for section in ("# Autonomous Ops Report", "## Executive summary", "## Headline KPIs", "## Decisions per agent",
                    "## Incidents and actions", "## Money", "## Anomalies and auto-actions",
                    "## Human interventions: 0"):
        assert section in md
    assert "Generated 2026-11-20 06:00 UTC (11:30 IST)" in md
    assert "| Dispatch p99 | 0.75 ms |" in md
    assert "| Booking Intent | active | 10 | 80% |" in md
    assert "| Platform revenue | ₹47,968.00 |" in md
    assert "**high_fallback_rate** · booking_intent" in md and "→ alerted the Ops feed (ok)" in md
    assert "Pool Batching" not in md  # planned agents are not listed


def test_render_without_anomalies_and_with_llm_summary():
    rep = rp.build_report(inputs(), [], [], trigger="manual", report_id="opr_2",
                          summary="All quiet.", summary_source="llm")
    assert rep["summary"] == "All quiet." and "All quiet." in rep["markdown"]
    assert "No anomalies detected." in rep["markdown"]
    assert "_Summary written by the LLM" in rep["markdown"]


def test_template_summary_mentions_anomalies():
    inp = inputs(ledger={**LEDGER, "trial_balance_paise": -5, "balanced": False})
    rep = rp.build_report(inp, rp.detect_anomalies(inp), [], trigger="manual", report_id="opr_3")
    assert "1 anomaly" in rep["summary"] and "0 human interventions" in rep["summary"]
    assert rp.template_summary(rp.build_report(inputs(), [], [], "manual", "x")).lower().count("no anomalies") == 1


def test_helpers():
    assert rp.parse_ts("2026-11-20T05:00:00Z") == NOW - dt.timedelta(hours=1)
    assert rp.parse_ts("2026-11-20T05:00:00.123456+00:00").minute == 0
    assert rp.parse_ts(None) is None and rp.parse_ts(42) is None
    assert rp.rupees(123456) == "₹1,234.56" and rp.rupees(None) == "n/a"
    assert rp.pct(None) == "n/a" and rp.pct(0.123) == "12%"
