"""Autonomous Ops Report: pure anomaly rules and the deterministic report / markdown builder.

Anomalies (``detect_anomalies``), in this order:

* ``high_fallback_rate``  an agent with > 50 % fallbacks over >= 5 decisions in the last hour
* ``silent_agent``        an agent active in the last hour (not a purely event-driven one) with no
                          event for 10 min
* ``stale_incident``      a safety incident still open (check-in or escalated) after 15 min
* ``ledger_imbalance``    trial balance != 0

Everything here is deterministic; only the executive summary paragraph may later
be replaced by an LLM-written one (``summary_source = "llm"``).
"""
from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

FALLBACK_RATE_MAX = 0.5
MIN_DECISIONS = 5
SILENT_AFTER_S = 600
INCIDENT_STALE_S = 900
# Agents that only act when something happens (a booking, an incident, a regulation, a surge crossing):
# silence is normal for them.
EVENT_DRIVEN = frozenset({"booking_intent", "safety_sentinel", "legal_jurisdiction", "support_dispute",
                          "compliance_vision", "surge_equilibrium"})
OPEN_STATUSES = ("check_in", "escalated")
IST = dt.timezone(dt.timedelta(hours=5, minutes=30))


@dataclass
class ReportInputs:
    now: dt.datetime
    agents: List[Dict[str, Any]] = field(default_factory=list)   # GET /v1/agents rows
    metrics: Optional[Dict[str, Any]] = None                     # dispatch-go /v1/metrics/dispatch
    ledger: Optional[Dict[str, Any]] = None                      # dispatch-go /v1/ledger/summary
    incidents: List[Dict[str, Any]] = field(default_factory=list)
    budget: Optional[Dict[str, Any]] = None                      # LLM budget
    sources: Dict[str, str] = field(default_factory=dict)        # name -> "ok" | "unavailable: ..."


# ---------------------------------------------------------------- formatting helpers

def parse_ts(v: Any) -> Optional[dt.datetime]:
    if not isinstance(v, str):
        return None
    try:
        t = dt.datetime.fromisoformat(v.replace("Z", "+00:00"))
    except ValueError:
        return None
    return t if t.tzinfo else t.replace(tzinfo=dt.timezone.utc)


def iso(t: dt.datetime) -> str:
    return t.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def rupees(paise: Any) -> str:
    if paise is None:
        return "n/a"
    sign = "-" if paise < 0 else ""
    return f"{sign}₹{abs(paise) / 100:,.2f}"


def pct(v: Any) -> str:
    return "n/a" if v is None else f"{v:.0%}"


def _na(v: Any, unit: str = "") -> str:
    if v is None:
        return "n/a"
    if isinstance(v, float):
        return f"{v:,.2f}{unit}"
    return f"{v:,}{unit}" if isinstance(v, int) else f"{v}{unit}"


def _last_ts(a: Dict[str, Any]) -> Optional[dt.datetime]:
    return parse_ts((a.get("last_decision") or {}).get("ts"))


# ---------------------------------------------------------------- anomalies

def detect_anomalies(inp: ReportInputs) -> List[Dict[str, Any]]:
    out: List[Dict[str, Any]] = []
    active = [a for a in inp.agents if a.get("status") == "active"]
    for a in active:
        n, rate = int(a.get("decisions_last_hour") or 0), float(a.get("fallback_rate") or 0.0)
        if n >= MIN_DECISIONS and rate > FALLBACK_RATE_MAX:
            out.append({"kind": "high_fallback_rate", "subject": a["id"], "severity": "warning",
                        "detail": f"fallback rate {rate:.0%} over {n} decisions in the last hour "
                                  f"(limit {FALLBACK_RATE_MAX:.0%})"})
    for a in active:
        last = _last_ts(a)
        if a["id"] in EVENT_DRIVEN or not a.get("decisions_last_hour") or last is None:
            continue
        idle_s = (inp.now - last).total_seconds()
        if idle_s > SILENT_AFTER_S:
            out.append({"kind": "silent_agent", "subject": a["id"], "severity": "warning",
                        "detail": f"no events for {int(idle_s // 60)} min (last at {last:%H:%M} UTC); "
                                  "active in the last hour"})
    for inc in inp.incidents:
        created = parse_ts(inc.get("created_at"))
        if inc.get("status") not in OPEN_STATUSES or created is None:
            continue
        age_s = (inp.now - created).total_seconds()
        if age_s > INCIDENT_STALE_S:
            out.append({"kind": "stale_incident", "subject": inc.get("id"), "severity": "critical",
                        "driver_id": inc.get("driver_id"),
                        "detail": f"{inc.get('status')} incident for driver {inc.get('driver_id') or '?'} "
                                  f"open {int(age_s // 60)} min (limit {INCIDENT_STALE_S // 60})"})
    led = inp.ledger
    if led is not None and (led.get("trial_balance_paise") not in (0, None) or led.get("balanced") is False):
        tb = led.get("trial_balance_paise")
        out.append({"kind": "ledger_imbalance", "subject": "ledger", "severity": "critical",
                    "detail": (f"trial balance is {rupees(tb)}, not ₹0.00" if tb not in (0, None)
                               else "ledger reports itself unbalanced")})
    return out


# ---------------------------------------------------------------- report

def _kpis(inp: ReportInputs) -> Dict[str, Any]:
    m, b = inp.metrics or {}, inp.budget or {}
    rows = list(inp.agents)
    decisions = sum(int(a.get("decisions_last_hour") or 0) for a in rows)
    fallbacks = sum(int(a.get("decisions_last_hour") or 0) * float(a.get("fallback_rate") or 0) for a in rows)
    return {
        "decisions_today": m.get("decisions_today"),
        "decisions_last_hour": decisions,
        "fallback_rate": round(fallbacks / decisions, 4) if decisions else 0.0,
        "matches_today": m.get("matches_today"),
        "rides_settled_today": m.get("rides_settled_today", (inp.ledger or {}).get("rides_settled_today")),
        "rides_in_progress": m.get("rides_in_progress"),
        "active_drivers": m.get("active_drivers"),
        "idle_drivers": m.get("idle_drivers"),
        "open_requests": m.get("open_requests"),
        "dispatch_p99_ms": m.get("p99_ms"),
        "llm_calls_today": b.get("calls_today"),
        "llm_tokens_today": b.get("tokens_today"),
        "active_agents": sum(1 for a in rows if a.get("status") == "active"),
        "total_agents": len(rows),
    }


def _incidents(inp: ReportInputs) -> Dict[str, Any]:
    recent = []
    for inc in inp.incidents:
        created = parse_ts(inc.get("created_at"))
        age = int((inp.now - created).total_seconds() // 60) if created else None
        recent.append({"id": inc.get("id"), "status": inc.get("status"), "decision": inc.get("decision"),
                       "driver_id": inc.get("driver_id"), "age_min": age})
    open_ = [i for i in recent if i["status"] in OPEN_STATUSES]
    return {
        "total": len(recent),
        "open": len(open_),
        "escalated": sum(1 for i in recent if i["status"] == "escalated"),
        "stale": sum(1 for i in open_ if i["age_min"] is not None and i["age_min"] * 60 > INCIDENT_STALE_S),
        "recent": recent[:5],
    }


def _money(inp: ReportInputs) -> Optional[Dict[str, Any]]:
    led = inp.ledger
    if led is None:
        return None
    po = led.get("payouts_today") or {}
    return {
        "trial_balance_paise": led.get("trial_balance_paise"),
        "balanced": led.get("balanced"),
        "platform_revenue_paise": led.get("platform_revenue_paise"),
        "rides_settled_today": led.get("rides_settled_today"),
        "payouts_sent_count": po.get("sent_count"),
        "payouts_sent_paise": po.get("sent_paise"),
        "held_count": po.get("held_count"),
        "held_paise": po.get("held_paise"),
    }


def template_summary(rep: Dict[str, Any]) -> str:
    k, money, anomalies = rep["kpis"], rep.get("money"), rep["anomalies"]
    ledger = ("the ledger is unavailable" if money is None
              else "the ledger balances to ₹0.00" if money.get("trial_balance_paise") == 0
              else f"the ledger is off by {rupees(money.get('trial_balance_paise'))}")
    if anomalies:
        kinds = ", ".join(sorted({a["kind"].replace("_", " ") for a in anomalies}))
        found = (f"{len(anomalies)} anomal{'y' if len(anomalies) == 1 else 'ies'} ({kinds}) "
                 "flagged and handled automatically")
    else:
        found = "no anomalies detected"
    return (f"In the last hour {k['active_agents']} active agents made {k['decisions_last_hour']:,} decisions "
            f"({pct(k['fallback_rate'])} on deterministic fallbacks); {_na(k['matches_today'])} matches and "
            f"{_na(k['rides_settled_today'])} settled rides today, and {ledger}. "
            f"{found[0].upper() + found[1:]}; 0 human interventions.")


def build_report(inp: ReportInputs, anomalies: List[Dict[str, Any]], actions: List[Dict[str, Any]],
                 trigger: str, report_id: str, summary: Optional[str] = None,
                 summary_source: str = "template") -> Dict[str, Any]:
    rep: Dict[str, Any] = {
        "id": report_id,
        "generated_at": iso(inp.now),
        "trigger": trigger,
        "kpis": _kpis(inp),
        "agents": [{"id": a["id"], "name": a.get("name") or a["id"], "status": a.get("status"),
                    "decisions_last_hour": int(a.get("decisions_last_hour") or 0),
                    "fallback_rate": float(a.get("fallback_rate") or 0.0),
                    "last_ts": (a.get("last_decision") or {}).get("ts")} for a in inp.agents],
        "incidents": _incidents(inp),
        "money": _money(inp),
        "anomalies": anomalies,
        "actions": actions,
        "human_interventions": 0,
        "sources": dict(inp.sources),
    }
    rep["summary"] = summary or template_summary(rep)
    rep["summary_source"] = summary_source if summary else "template"
    rep["markdown"] = render_markdown(rep)
    return rep


def render_markdown(rep: Dict[str, Any]) -> str:
    t = parse_ts(rep["generated_at"]) or dt.datetime.now(dt.timezone.utc)
    k = rep["kpis"]
    lines = [
        "# Autonomous Ops Report",
        f"_Generated {t:%Y-%m-%d %H:%M} UTC ({t.astimezone(IST):%H:%M} IST) · trigger: {rep['trigger']} · "
        f"Ops Supervisor · report {rep['id']}_",
        "",
        "## Executive summary",
        rep["summary"],
    ]
    if rep.get("summary_source") == "llm":
        lines.append(f"_Summary written by the LLM{' (' + rep['summary_model'] + ')' if rep.get('summary_model') else ''}"
                     "; every figure below is computed deterministically._")
    lines += [
        "",
        "## Headline KPIs",
        "| KPI | Value |",
        "|---|---|",
        f"| Autonomous decisions today | {_na(k['decisions_today'])} |",
        f"| Decisions in the last hour | {_na(k['decisions_last_hour'])} |",
        f"| Fallback rate (last hour) | {pct(k['fallback_rate'])} |",
        f"| Matches today | {_na(k['matches_today'])} |",
        f"| Rides settled today | {_na(k['rides_settled_today'])} |",
        f"| Rides in progress | {_na(k['rides_in_progress'])} |",
        f"| Drivers active / idle | {_na(k['active_drivers'])} / {_na(k['idle_drivers'])} |",
        f"| Dispatch p99 | {_na(k['dispatch_p99_ms'], ' ms')} |",
        f"| LLM calls / tokens today | {_na(k['llm_calls_today'])} / {_na(k['llm_tokens_today'])} |",
        f"| Agents active | {k['active_agents']} of {k['total_agents']} |",
        "",
        "## Decisions per agent (last hour)",
        "| Agent | Status | Decisions | Fallback rate | Last decision |",
        "|---|---|---|---|---|",
    ]
    for a in sorted((a for a in rep["agents"] if a["status"] == "active"), key=lambda a: -a["decisions_last_hour"]):
        last = parse_ts(a.get("last_ts"))
        lines.append(f"| {a['name']} | {a['status']} | {a['decisions_last_hour']:,} | {pct(a['fallback_rate'])} | "
                     f"{f'{last:%H:%M} UTC' if last else 'never'} |")
    inc = rep["incidents"]
    lines += ["", "## Incidents and actions",
              f"- {inc['total']} recent safety incidents: {inc['open']} open, {inc['escalated']} escalated, "
              f"{inc['stale']} open longer than {INCIDENT_STALE_S // 60} min"]
    for i in inc["recent"]:
        age = f"{i['age_min']} min ago" if i["age_min"] is not None else "time unknown"
        lines.append(f"  - `{i['id']}` {i['status']} ({i['decision']}), driver {i['driver_id'] or '?'}, {age}")
    lines += ["", "## Money"]
    m = rep.get("money")
    if m is None:
        lines.append("Ledger summary unavailable.")
    else:
        lines += [
            "| Item | Value |",
            "|---|---|",
            f"| Trial balance | {rupees(m['trial_balance_paise'])} ({'balanced' if m['balanced'] else 'UNBALANCED'}) |",
            f"| Platform revenue | {rupees(m['platform_revenue_paise'])} |",
            f"| Rides settled today | {_na(m['rides_settled_today'])} |",
            f"| Payouts sent today | {_na(m['payouts_sent_count'])} ({rupees(m['payouts_sent_paise'])}) |",
            f"| Payouts held for review | {_na(m['held_count'])} ({rupees(m['held_paise'])}) |",
        ]
    lines += ["", "## Anomalies and auto-actions"]
    if not rep["anomalies"]:
        lines.append("No anomalies detected.")
    acts = {(a["kind"], a["subject"]): a for a in rep["actions"]}
    for a in rep["anomalies"]:
        act = acts.get((a["kind"], a["subject"]))
        tail = f" → {act['action']} ({act['result']})" if act else ""
        lines.append(f"- **{a['kind']}** · {a['subject']}: {a['detail']}{tail}")
    lines += ["", f"## Human interventions: {rep['human_interventions']}",
              "Every decision and action above was taken by an agent; no human override was needed."]
    missing = [f"{name} {state}" for name, state in rep.get("sources", {}).items() if state != "ok"]
    if missing:
        lines += ["", f"_Data sources: {' · '.join(missing)}_"]
    return "\n".join(lines) + "\n"
