"""Offline tests for tools/smoke.py: a fake gateway stands in for HTTP and the WebSocket."""
import json

import pytest

import smoke
from smoke import CheckFailed, Ctx, Response


def resp(status=200, body=None, headers=None):
    raw = body if isinstance(body, bytes) else json.dumps(body if body is not None else {}).encode()
    return Response(status, headers or {}, raw)


class FakeClock:
    def __init__(self):
        self.t = 0.0

    def __call__(self):
        return self.t

    def sleep(self, s):
        self.t += s


class Gateway:
    """Routes (method, path-without-host) to handlers; records every call."""

    def __init__(self, routes):
        self.routes = routes
        self.calls = []

    def __call__(self, method, url, body=None, headers=None, timeout=60):
        path = url.split("://", 1)[1]
        path = path[path.index("/"):] if "/" in path else "/"
        self.calls.append((method, path, body, headers))
        for (m, p), h in self.routes.items():
            if m == method and (path == p or (p.endswith("*") and path.startswith(p[:-1]))):
                return h(body, headers) if callable(h) else h
        return resp(404, {"error": "not found"})


def ctx_for(routes, **kw):
    clock = FakeClock()
    gw = Gateway(routes)
    c = Ctx(gateway="https://gw", agents="https://ag", web="https://web", http=gw, clock=clock,
            sleep=clock.sleep, cold_start_s=20, **kw)
    return c, gw


def plan(fallback=False, legs=2):
    return {"type": "plan", "legs": [{}] * legs, "total_fare": 1208,
            "answered_by": {"model": None if fallback else "gemini/x", "label": "x", "used_fallback": fallback,
                            "fallback_reason": "forced" if fallback else None}}


# ---------------------------------------------------------------- health


def test_health_waits_through_cold_start():
    n = {"i": 0}

    def flaky(body, headers):
        n["i"] += 1
        if n["i"] == 1:
            raise ConnectionResetError
        return resp(503 if n["i"] == 2 else 200)

    c, _ = ctx_for({("GET", "/healthz"): flaky})
    assert smoke.check_health_gateway(c) == "200 after 10.0 s"


def test_health_gives_up_after_cold_start_window():
    c, _ = ctx_for({("GET", "/"): resp(502)})
    with pytest.raises(CheckFailed, match=r"not healthy after 20 s \(HTTP 502\)"):
        smoke.check_health_web(c)


def test_agents_health_uses_agents_url():
    c, gw = ctx_for({("GET", "/healthz"): resp(200)})
    smoke.check_health_agents(c)
    assert gw.calls[0][1] == "/healthz"


# ---------------------------------------------------------------- dispatch, rides, stream


def test_dispatch_p99_in_microseconds():
    c, _ = ctx_for({("GET", "/v1/metrics/dispatch"): resp(body={"p50_ms": 0.12, "p99_ms": 0.6, "active_drivers": 300})})
    assert smoke.check_dispatch_p99(c) == "p50 120 µs, p99 600 µs, 300 drivers"


@pytest.mark.parametrize("m,msg", [({"p50_ms": 1}, "no p99_ms"), ({"p50_ms": 1, "p99_ms": 80}, "p99 80 ms > 50")])
def test_dispatch_p99_failures(m, msg):
    c, _ = ctx_for({("GET", "/v1/metrics/dispatch"): resp(body=m)})
    with pytest.raises(CheckFailed, match=msg):
        smoke.check_dispatch_p99(c)


def test_ride_matched_immediately():
    ride = {"id": "abcdef123", "driver_id": "d_1", "status": "matched", "eta_seconds": 120.4}
    c, _ = ctx_for({("POST", "/v1/rides"): resp(201, {"ride": ride})})
    assert smoke.check_ride_matched(c) == "abcdef12 -> d_1 (matched, ETA 120 s)"
    assert c.state["ride_id"] == "abcdef123"


def test_ride_waiting_then_matched():
    n = {"i": 0}

    def lookup(body, headers):
        n["i"] += 1
        return resp(body={"ride": {"id": "r1", "driver_id": "d_9" if n["i"] > 1 else "", "status": "matched"}})

    c, _ = ctx_for({("POST", "/v1/rides"): resp(202, {"ride": {"id": "r1", "status": "requested"}}),
                    ("GET", "/v1/rides/r1"): lookup})
    assert "-> d_9" in smoke.check_ride_matched(c)


def test_ride_never_matched():
    c, _ = ctx_for({("POST", "/v1/rides"): resp(202, {"ride": {"id": "r1"}}),
                    ("GET", "/v1/rides/r1"): resp(body={"id": "r1"})})
    with pytest.raises(CheckFailed, match="not matched within 30 s"):
        smoke.check_ride_matched(c)


def test_ride_rejected():
    c, _ = ctx_for({("POST", "/v1/rides"): resp(400, {"error": "bad"})})
    with pytest.raises(CheckFailed, match="HTTP 400"):
        smoke.check_ride_matched(c)


def test_stream_subscribes_to_drivers_and_the_ride():
    seen = {}

    def ws(url, topics, timeout):
        seen.update(url=url, topics=topics, timeout=timeout)
        return {"topic": "drivers", "data": [{"id": "d_1"}]}

    c, _ = ctx_for({}, ws=ws)
    c.state["ride_id"] = "r1"
    assert smoke.check_stream(c).startswith("drivers frame after")
    assert seen == {"url": "wss://gw/v1/stream", "topics": ["drivers", "ride:r1"], "timeout": 5.0}


@pytest.mark.parametrize("frame,want", [
    ({"topic": "ride:r1", "data": {"driver": {"lat": 1, "lng": 2}}}, True),
    ({"topic": "ride:r1", "data": {"driver": {}}}, False),
    ({"topic": "drivers", "data": [{"id": "d"}]}, True),
    ({"topic": "drivers", "data": {"drivers": [{"id": "d"}]}}, True),
    ({"topic": "drivers", "data": []}, False),
    ({"topic": "ops", "data": {"type": "metrics"}}, False),
])
def test_driver_in_frame(frame, want):
    assert smoke.driver_in_frame(frame) is want


def test_websocket_timeout_is_a_failed_check(monkeypatch):
    async def never(url, topics, timeout):
        raise TimeoutError

    monkeypatch.setattr(smoke, "_ws_first_driver_frame", never)
    with pytest.raises(CheckFailed, match="no driver update within 5 s"):
        smoke.websockets_first_driver_frame("wss://x", ["drivers"], 5)


# ---------------------------------------------------------------- booking, pricing


def test_booking_golden_by_llm():
    c, gw = ctx_for({("POST", "/v1/agents/booking/parse"): resp(body=plan())})
    assert smoke.check_booking_golden(c) == "2 legs, ₹1208, by x"
    assert gw.calls[0][2]["text"] == smoke.GOLDEN_SENTENCE


def test_booking_force_fallback_uses_the_query_flag():
    c, gw = ctx_for({("POST", "/v1/agents/booking/parse?force_fallback=true"): resp(body=plan(fallback=True))})
    assert smoke.check_booking_fallback(c) == "2 legs, ₹1208, by rules (forced)"


def test_booking_force_fallback_must_skip_the_llm():
    c, _ = ctx_for({("POST", "/v1/agents/booking/parse?force_fallback=true"): resp(body=plan())})
    with pytest.raises(CheckFailed, match="force_fallback but answered by gemini/x"):
        smoke.check_booking_fallback(c)


@pytest.mark.parametrize("body,msg", [
    ({"type": "clarify", "question": "Which office?"}, "type=clarify: Which office"),
    (plan(legs=1), "1 legs, want 2"),
])
def test_booking_wrong_plan(body, msg):
    c, _ = ctx_for({("POST", "/v1/agents/booking/parse"): resp(body=body)})
    with pytest.raises(CheckFailed, match=msg):
        smoke.check_booking_golden(c)


def test_pricing_quote():
    c, _ = ctx_for({("POST", "/v1/pricing/quote"): resp(body={"total": 260, "distance_km": 6.7,
                                                              "surge_multiplier": 1.0})})
    assert smoke.check_pricing_quote(c) == "sedan ₹260 (6.7 km, 1.0x)"


def test_pricing_quote_zero():
    c, _ = ctx_for({("POST", "/v1/pricing/quote"): resp(body={"total": 0})})
    with pytest.raises(CheckFailed, match="total=0"):
        smoke.check_pricing_quote(c)


def test_surge_map():
    c, _ = ctx_for({("GET", "/v1/pricing/surge-map"): resp(body={"cells": [{}, {}], "surging": 1,
                                                                 "max_multiplier": 1.5})})
    assert smoke.check_surge_map(c) == "2 cells, 1 surging, max 1.5x"
    c, _ = ctx_for({("GET", "/v1/pricing/surge-map"): resp(body={"cells": []})})
    with pytest.raises(CheckFailed, match="no cells"):
        smoke.check_surge_map(c)


def test_not_json_is_a_failure():
    c, _ = ctx_for({("GET", "/v1/pricing/surge-map"): resp(body=b"<html>")})
    with pytest.raises(CheckFailed, match="not JSON"):
        smoke.check_surge_map(c)


# ---------------------------------------------------------------- legal & ops report


def legal_routes(old_cap=2.5, maps=None, enabled=True, new_cap=1.5):
    state = {"maps": list(maps or [{"as_of": "t0", "max_multiplier": 2.5}, {"as_of": "t1", "max_multiplier": 1.5}]),
             "ingests": []}

    def surge(body, headers):
        m = state["maps"][0] if len(state["maps"]) == 1 else state["maps"].pop(0)
        return resp(body=m)

    def ingest(body, headers):
        state["ingests"].append(body)
        cap = new_cap if len(state["ingests"]) == 1 else old_cap
        return resp(body={"source": "llm", "model": "gemini/x", "diff": [], "jurisdiction": {
            "rules": {"max_surge_multiplier": cap}}})

    routes = {
        ("GET", "/v1/legal/jurisdictions"): resp(body={"enabled": enabled, "pricing_city": "bengaluru", "jurisdictions": [
            {"city": "bengaluru", "rules": {"max_surge_multiplier": old_cap}}]}),
        ("POST", "/v1/legal/ingest"): ingest,
        ("GET", "/v1/pricing/surge-map"): surge,
    }
    return routes, state


def test_legal_cap_applies_to_surge_map_and_is_restored():
    routes, state = legal_routes()
    c, _ = ctx_for(routes)
    assert smoke.check_legal_surge_cap(c) == ("llm (gemini/x): cap 2.5x -> 1.5x, surge-map max 1.5x "
                                              "after recompute, restored 2.5x")
    assert state["ingests"][0] == {"city": "bengaluru", "text": smoke.SAMPLE_NOTICE}
    assert "surge cap of 2.5x" in state["ingests"][1]["text"]


def test_legal_cap_keep_and_already_at_cap():
    routes, state = legal_routes(old_cap=1.5)
    c, _ = ctx_for(routes)
    assert smoke.check_legal_surge_cap(c).endswith("after recompute")
    assert len(state["ingests"]) == 1  # nothing to restore
    routes, state = legal_routes()
    c, _ = ctx_for(routes, restore_legal=False)
    assert smoke.check_legal_surge_cap(c).endswith("after recompute, kept")
    assert len(state["ingests"]) == 1


def test_legal_cap_failures():
    routes, _ = legal_routes(enabled=False)
    c, _ = ctx_for(routes)
    with pytest.raises(CheckFailed, match="LEGAL_ENABLED is off"):
        smoke.check_legal_surge_cap(c)
    routes, _ = legal_routes(new_cap=2.0)
    c, _ = ctx_for(routes)
    with pytest.raises(CheckFailed, match="cap after ingest 2.0, want 1.5"):
        smoke.check_legal_surge_cap(c)
    routes, state = legal_routes(maps=[{"as_of": "t0", "max_multiplier": 2.5}, {"as_of": "t1", "max_multiplier": 2.5}])
    c, _ = ctx_for(routes)
    with pytest.raises(CheckFailed, match="surge-map max still above 1.5x"):
        smoke.check_legal_surge_cap(c)
    assert len(state["ingests"]) == 2  # restored even on failure
    routes, _ = legal_routes()
    routes[("GET", "/v1/legal/jurisdictions")] = resp(body={"enabled": True, "jurisdictions": []})
    c, _ = ctx_for(routes)
    with pytest.raises(CheckFailed, match="no jurisdiction bengaluru"):
        smoke.check_legal_surge_cap(c)


def test_ops_report():
    rep = {"id": "opr_1", "generated_at": "2026-11-20T06:00:00Z", "anomalies": [{}], "actions": [{}],
           "summary_source": "template", "markdown": "# Autonomous Ops Report\n## Human interventions: 0\n"}
    c, _ = ctx_for({("POST", "/v1/ops/report/run"): resp(body=rep), ("GET", "/v1/ops/report"): resp(body=rep)})
    assert smoke.check_ops_report(c) == "opr_1: 1 anomalies, 1 auto-actions, 0 human interventions"
    bad = {**rep, "markdown": "# nope"}
    c, _ = ctx_for({("POST", "/v1/ops/report/run"): resp(body=bad), ("GET", "/v1/ops/report"): resp(body=bad)})
    with pytest.raises(CheckFailed, match="Human interventions: 0"):
        smoke.check_ops_report(c)
    old = {**rep, "id": "opr_0", "generated_at": "2026-11-20T05:00:00Z"}
    c, _ = ctx_for({("POST", "/v1/ops/report/run"): resp(body=rep), ("GET", "/v1/ops/report"): resp(body=old)})
    with pytest.raises(CheckFailed, match="latest report opr_0 is older"):
        smoke.check_ops_report(c)


def test_main_keep_legal_flag():
    assert smoke.parse_args(["--keep-legal"]).keep_legal is True
    assert smoke.parse_args([]).keep_legal is False



# ---------------------------------------------------------------- safety


def safety_routes(incidents, reply_status=200):
    seq = iter(incidents)
    last = {"v": []}

    def lst(body, headers):
        last["v"] = next(seq, last["v"])
        return resp(body={"incidents": last["v"]})

    return {("POST", "/v1/sim/scenario/safety_incident"): resp(body={"ride_id": "ride1234x"}),
            ("GET", "/v1/safety/incidents?limit=200"): lst,
            ("POST", "/v1/safety/checkins/*"): resp(reply_status, {"ok": True})}


def inc(**kw):
    return {"id": "inc_1", "ride_id": "ride1234x", "driver_id": "d_7", **kw}


def test_safety_reply_im_fine():
    c, gw = ctx_for(safety_routes([[], [inc(status="check_in", pending_checkin_id="chk_1")],
                                   [inc(status="monitoring", decision="monitor", pending_checkin_id=None)]]))
    assert smoke.check_safety(c) == "inc_1: check-in answered 'I'm fine' -> monitoring"
    reply = [x for x in gw.calls if x[1] == "/v1/safety/checkins/chk_1/reply"]
    assert reply[0][2] == {"transcript": "I'm fine"}


def test_safety_reply_not_accepted():
    c, _ = ctx_for(safety_routes([[inc(pending_checkin_id="chk_1")],
                                  [inc(status="escalated", decision="escalate", reason="timeout")]]))
    with pytest.raises(CheckFailed, match="after 'I'm fine': escalate"):
        smoke.check_safety(c)


def test_safety_let_it_escalate():
    c, gw = ctx_for(safety_routes([[inc(status="check_in", pending_checkin_id="chk_1")],
                                   [inc(status="escalated", flags={"driver_frozen": True})]]),
                    safety_reply="escalate")
    assert smoke.check_safety(c) == "inc_1: escalated, driver d_7 frozen"
    assert not any("/reply" in x[1] for x in gw.calls)


def test_safety_escalated_without_freeze():
    c, _ = ctx_for(safety_routes([[inc(status="escalated", flags={})]]), safety_reply="escalate")
    with pytest.raises(CheckFailed, match="driver not frozen"):
        smoke.check_safety(c)


def test_safety_no_incident():
    c, _ = ctx_for(safety_routes([[]]))
    with pytest.raises(CheckFailed, match="no incident for ride ride1234 within 90 s"):
        smoke.check_safety(c)


def test_safety_scenario_without_ride():
    c, _ = ctx_for({("POST", "/v1/sim/scenario/safety_incident"): resp(body={"detail": "no ride on trip"})})
    with pytest.raises(CheckFailed, match="no ride on trip"):
        smoke.check_safety(c)


# ---------------------------------------------------------------- ledger, keys, docs, release


def test_trial_balance():
    c, _ = ctx_for({("GET", "/v1/ledger/trial-balance"): resp(body={"total_paise": 0, "balanced": True,
                                                                    "types": [{"accounts": 2}, {"accounts": 3}]})})
    assert smoke.check_trial_balance(c) == "balanced, ₹0.00 across 5 accounts"
    c, _ = ctx_for({("GET", "/v1/ledger/trial-balance"): resp(body={"total_paise": 5, "balanced": False})})
    with pytest.raises(CheckFailed, match="total_paise=5"):
        smoke.check_trial_balance(c)


def key_routes(summary_headers=None, rides_status=403):
    return {("POST", "/v1/developer/keys"): resp(201, {"api_key": "hxg_secret", "prefix": "hxg_ab"}),
            ("GET", "/v1/ledger/summary"): resp(body={}, headers=summary_headers if summary_headers is not None
                                                 else {"x-ratelimit-limit": "60"}),
            ("GET", "/v1/rides/none"): resp(rides_status, {"error": "x"}),
            ("DELETE", "/v1/developer/keys/me"): resp(204, b"")}


def test_api_key_scoped_call_and_revoke():
    c, gw = ctx_for(key_routes())
    assert smoke.check_api_key(c) == "hxg_ab: ledger summary 200 (limit 60/min), rides 403, revoked"
    assert gw.calls[1][3] == {"Authorization": "Bearer hxg_secret"}
    assert gw.calls[-1][:2] == ("DELETE", "/v1/developer/keys/me")


@pytest.mark.parametrize("routes,msg", [(key_routes(summary_headers={}), "no X-RateLimit-Limit"),
                                        (key_routes(rides_status=404), "answered 404, want 403")])
def test_api_key_failures_still_revoke(routes, msg):
    c, gw = ctx_for(routes)
    with pytest.raises(CheckFailed, match=msg):
        smoke.check_api_key(c)
    assert gw.calls[-1][:2] == ("DELETE", "/v1/developer/keys/me")


def test_forecast_heatmap():
    cell = {"h3": "a", "boundary": [[1, 2]], "explanation": "x", "expected_earnings_per_hr": 412,
            "explanation_source": "llm"}
    c, gw = ctx_for({("GET", "/v1/forecast/heatmap*"): resp(body={"cells": [cell, {"expected_earnings_per_hr": 1}],
                                                                  "source": "postgres"})})
    assert smoke.check_forecast_heatmap(c) == "2 cells at +15 min from postgres, top ₹412/h (llm)"
    assert gw.calls[0][1] == "/v1/forecast/heatmap?horizon=15"
    c, _ = ctx_for({("GET", "/v1/forecast/heatmap*"): resp(body={"cells": [], "source": "none"})})
    with pytest.raises(CheckFailed, match="no cells"):
        smoke.check_forecast_heatmap(c)
    c, _ = ctx_for({("GET", "/v1/forecast/heatmap*"): resp(body={"cells": [{"h3": "b"}]})})
    with pytest.raises(CheckFailed, match="without boundary"):
        smoke.check_forecast_heatmap(c)


def takeover_routes(send=None, offer=True, decision=None, ride=True):
    drivers = {"drivers": [{"id": "d_001", "status": "on_trip"}, {"id": "d_002", "status": "idle"},
                           {"id": "d_003", "status": "idle"}]}
    o = {"ride_id": "ride-1234-x", "fare_inr": 214, "pickup_eta_sec": 90} if offer else None
    return {
        ("GET", "/v1/drivers"): resp(body=drivers),
        ("POST", "/v1/drivers/d_002/takeover"): resp(body={"human_controlled": True}),
        ("POST", "/v1/drivers/d_003/takeover"): resp(body={"human_controlled": True}),
        ("POST", "/v1/drivers/d_002/send-ride"): send or resp(409, {"error": "busy"}),
        ("POST", "/v1/drivers/d_003/send-ride"): resp(201, {"ride": {"id": "ride-1234-x"}}),
        ("GET", "/v1/drivers/d_003/takeover"): resp(body={"offer": o}),
        ("POST", "/v1/rides/ride-1234-x/offer/accept"): resp(body=decision or {"decision": "accepted"}),
        ("POST", "/v1/rides/ride-1234-x/offer/decline"): resp(body=decision or {"decision": "declined",
                                                                                 "rematched_to": "d_077"}),
        ("GET", "/v1/drivers/d_003"): resp(body={"ride": {"id": "ride-1234-x", "status": "accepted"} if ride else None}),
        ("DELETE", "/v1/drivers/d_002/takeover"): resp(body={}),
        ("DELETE", "/v1/drivers/d_003/takeover"): resp(body={}),
    }


def _released(gw, driver):
    return ("DELETE", f"/v1/drivers/{driver}/takeover") in [(m, p) for m, p, *_ in gw.calls]


def test_takeover_accept_skips_a_busy_driver_and_releases():
    c, gw = ctx_for(takeover_routes())
    assert smoke.check_takeover_accept(c) == "d_003 accepted ride-123 (₹214, pickup in 90 s) -> accepted"
    assert _released(gw, "d_002") and _released(gw, "d_003")


def test_takeover_decline_is_rematched():
    c, gw = ctx_for(takeover_routes())
    assert smoke.check_takeover_decline(c) == "d_003 declined ride-123 -> re-matched to d_077"
    c, gw = ctx_for(takeover_routes(decision={"decision": "declined", "rematched_to": ""}))
    with pytest.raises(CheckFailed, match="not re-matched"):
        smoke.check_takeover_decline(c)
    assert _released(gw, "d_003")


@pytest.mark.parametrize("kw, msg", [
    ({"offer": False}, "no offer"),
    ({"decision": {"decision": "nope"}}, "accept"),
    ({"ride": False}, "is not driving"),
    ({"send": resp(503, {"error": "disabled"})}, "send-ride HTTP 503"),
])
def test_takeover_accept_failures(kw, msg):
    c, gw = ctx_for(takeover_routes(**kw))
    with pytest.raises(CheckFailed, match=msg):
        smoke.check_takeover_accept(c)


def test_takeover_needs_an_idle_driver():
    c, _ = ctx_for({("GET", "/v1/drivers"): resp(body={"drivers": [{"id": "d_1", "status": "on_trip"}]})})
    with pytest.raises(CheckFailed, match="no idle driver"):
        smoke.check_takeover_accept(c)
    routes = takeover_routes()
    routes[("POST", "/v1/drivers/d_003/send-ride")] = resp(409, {"error": "busy"})
    c, _ = ctx_for(routes)
    with pytest.raises(CheckFailed, match="every idle driver"):
        smoke.check_takeover_accept(c)


def test_docs():
    spec = {"openapi": "3.1.0", "paths": {"/a": {"get": {}, "post": {}}, "/b": {"get": {}, "parameters": []}}}
    c, _ = ctx_for({("GET", "/docs"): resp(body=b"<div id=swagger-ui>"), ("GET", "/openapi.json"): resp(body=spec)})
    assert smoke.check_docs(c) == "/docs 200, OpenAPI 3.1.0 with 3 operations"
    c, _ = ctx_for({("GET", "/docs"): resp(404, b"")})
    with pytest.raises(CheckFailed, match="/docs HTTP 404"):
        smoke.check_docs(c)


def release_routes(assets):
    return {("GET", "/repos/o/r/releases/latest"): lambda b, h: resp(body={"tag_name": "v1", "assets": assets,
                                                                           "auth": h.get("Authorization")})}


def test_release_assets():
    c, gw = ctx_for(release_routes([{"name": "HexaGo-windows-x64.zip", "size": 13e6},
                                    {"name": "HexaGo-android.apk", "size": 56e6}]), repo="o/r", github_token="t")
    assert smoke.check_release(c) == "v1: HexaGo-windows-x64.zip 13 MB, HexaGo-android.apk 56 MB"
    assert gw.calls[0][3]["Authorization"] == "Bearer t"


def test_release_missing_asset():
    c, _ = ctx_for(release_routes([{"name": "HexaGo-android.apk", "size": 1}]), repo="o/r")
    with pytest.raises(CheckFailed, match=r"missing \['HexaGo-windows-x64.zip'\]"):
        smoke.check_release(c)


# ---------------------------------------------------------------- runner


def test_run_and_table_mark_failures_and_crashes():
    def boom(ctx):
        raise KeyError("ride")

    def bad(ctx):
        raise CheckFailed("nope")

    results = smoke.run(Ctx(), [("good", lambda ctx: "fine"), ("bad", bad), ("boom", boom)])
    assert [(r.name, r.ok, r.detail) for r in results] == [
        ("good", True, "fine"), ("bad", False, "nope"), ("boom", False, "KeyError: 'ride'")]
    text = smoke.table(results)
    assert "good  PASS" in text and "bad   FAIL" in text and "1/3 passed" in text


def test_main_exit_codes_and_json(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(smoke, "CHECKS", [("alpha", lambda ctx: ctx.gateway), ("beta", lambda ctx: "b")])
    out = tmp_path / "r.json"
    assert smoke.main(["--gateway", "https://x/", "--only", "alp", "--json", str(out)]) == 0
    assert json.loads(out.read_text(encoding="utf-8"))[0]["detail"] == "https://x"
    assert "1/1 passed" in capsys.readouterr().out

    def fail(ctx):
        raise CheckFailed("down")

    monkeypatch.setattr(smoke, "CHECKS", [("alpha", fail)])
    assert smoke.main([]) == 1


def test_args_from_environment(monkeypatch):
    monkeypatch.setenv("HEXAGO_GATEWAY_URL", "http://localhost:8080")
    monkeypatch.setenv("GITHUB_REPOSITORY", "me/fork")
    a = smoke.parse_args([])
    assert (a.gateway, a.repo, a.agents) == ("http://localhost:8080", "me/fork", smoke.AGENTS)


def test_urllib_http_handles_errors(monkeypatch):
    import urllib.error
    import urllib.request

    seen = {}

    class R:
        status = 200
        headers = {"X-A": "1"}

        def read(self):
            return b"{}"

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def fake_open(req, timeout):
        seen["ct"] = req.get_header("Content-type")
        if req.full_url.endswith("/err"):
            raise urllib.error.HTTPError(req.full_url, 418, "teapot", {"X-B": "2"}, None)
        return R()

    monkeypatch.setattr(urllib.request, "urlopen", fake_open)
    r = smoke.urllib_http("POST", "https://x/ok", {"a": 1})
    assert (r.status, r.headers, seen["ct"]) == (200, {"x-a": "1"}, "application/json")
    e = smoke.urllib_http("GET", "https://x/err")
    assert (e.status, e.headers) == (418, {"x-b": "2"})
