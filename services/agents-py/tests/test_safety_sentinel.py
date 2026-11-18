"""Safety Sentinel flows: every escalation path, guardrails, non-blocking LLM."""
import asyncio
import time

import pytest

from src.agents_py.agents.safety.decision import Confirmation
from src.agents_py.agents.safety.sentinel import (
    CheckinClosed, CheckinNotFound, IncidentNotFound, ResolutionRejected, SafetySentinel, emergency_contact,
)
from src.agents_py.agents.safety.store import ResolutionRefused
from tests.helpers import ScriptedProvider, make_llm
from tests.safety_fakes import FakeActions, FakeLLM, ManualSleep, make_sentinel, signal


async def settle(s: SafetySentinel) -> None:
    """Wait for background work except check-in timers that are still sleeping."""
    for _ in range(50):
        busy = [t for t in s._tasks if t not in s._timers.values() and not t.done()]
        if not busy:
            await asyncio.sleep(0)
            if not [t for t in s._tasks if t not in s._timers.values() and not t.done()]:
                return
            continue
        await asyncio.gather(*busy, return_exceptions=True)


def steps(s, inc_id):
    return [e["step"] for e in s.store.incidents[inc_id]["timeline"]]


# ---- signal -> decision ----

async def test_low_severity_only_monitors():
    s, rec, *_ = make_sentinel()
    out = await s.handle_signal(signal(0.2))
    assert out["action"] == "monitor" and out["incident_id"] is None
    assert s.store.incidents == {} and rec.kinds() == ["safety_monitor"]


async def test_severity_at_least_point_eight_escalates_immediately():
    actions = FakeActions()
    s, rec, *_ = make_sentinel(actions)
    out = await s.handle_signal(signal(0.85))
    assert out["action"] == "escalate" and "checkin_id" not in out
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "escalated" and not s.store.checkins  # nobody was asked
    assert {"emergency", "track", "suspend", "contact"} <= set(actions.names())
    flags = {k: inc["actions"][k]["status"] for k in ("emergency_notified", "location_shared", "driver_frozen",
                                                       "contact_notified")}
    assert set(flags.values()) == {"ok"}
    for want in ("signal_received", "context_gathered", "decision", "escalated", "emergency_dispatched",
                 "location_shared", "driver_suspended", "contact_notified", "rider_informed"):
        assert want in steps(s, inc["id"])
    assert all(e["at"] and e["actor"] for e in inc["timeline"])
    assert "safety_escalation" in rec.kinds()
    # The emergency payload carries location, trip, driver and vehicle.
    payload = next(c[1] for c in actions.calls if c[0] == "emergency_payload")
    assert payload["location"] == {"lat": 12.98, "lng": 77.6}
    assert payload["trip"]["ride_id"] == "ride1" and payload["driver"]["driver_id"] == "d_001"
    assert payload["vehicle"] == {"id": "d_001", "tier": "sedan", "autonomous": False}
    # The rider is told help is coming on the ride topic and ops.
    updates = [(t, d["status"]) for t, d in actions.published if d["type"] == "safety_update"]
    assert ("ride:ride1", "escalated") in updates and ("ops", "escalated") in updates


async def test_av_escalation_records_replacement_ride():
    actions = FakeActions(tier="av")
    s, *_ = make_sentinel(actions)
    out = await s.handle_signal(signal(0.9, tier="av"))
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["actions"]["replacement_ride"] == {"ride_id": "ride2"}
    assert "replacement_dispatched" in steps(s, inc["id"])
    payload = next(c[1] for c in actions.calls if c[0] == "emergency_payload")
    assert payload["vehicle"]["autonomous"] is True


async def test_check_in_pushes_safety_call_on_ride_and_ops():
    actions = FakeActions()
    s, rec, clock, sleep = make_sentinel(actions)
    out = await s.handle_signal(signal(0.46))
    await settle(s)
    assert out["action"] == "check_in" and out["checkin_id"]
    chk = s.store.checkins[out["checkin_id"]]
    assert chk["status"] == "pending" and chk["timeout_s"] == 15  # DEMO_MODE
    calls = [(t, d) for t, d in actions.published if d["type"] == "safety_call"]
    assert [t for t, _ in calls] == ["ride:ride1", "ops"]
    frame = calls[0][1]
    assert frame["checkin_id"] == chk["id"] and "Are you okay?" in frame["question"] and frame["timeout_s"] == 15
    assert sleep.waits == [15.0]
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "check_in" and inc["transcript"][0]["role"] == "agent"
    assert "call_pushed" in steps(s, inc["id"])
    # Production timeout is 30 s.
    s2, *_ = make_sentinel(demo=False)
    out2 = await s2.handle_signal(signal(0.5))
    assert s2.store.checkins[out2["checkin_id"]]["timeout_s"] == 30
    await settle(s2)


async def test_no_reply_escalates():
    actions = FakeActions()
    s, rec, clock, sleep = make_sentinel(actions)
    out = await s.handle_signal(signal(0.46))
    await settle(s)
    clock.advance(15)
    sleep.fire()
    await s.idle()
    inc = s.store.incidents[out["incident_id"]]
    assert s.store.checkins[out["checkin_id"]]["status"] == "timed_out"
    assert inc["status"] == "escalated" and "no reply" in inc["reason"]
    assert "checkin_timeout" in steps(s, inc["id"]) and "driver_suspended" in steps(s, inc["id"])
    assert inc["transcript"][-1]["role"] == "system"
    with pytest.raises(CheckinClosed):
        await s.handle_reply(out["checkin_id"], "I'm fine")  # too late


async def test_im_fine_monitors():
    actions = FakeActions()
    s, rec, clock, sleep = make_sentinel(actions)
    out = await s.handle_signal(signal(0.46))
    reply = await s.handle_reply(out["checkin_id"], "I'm fine, thanks")
    assert reply["action"] == "monitor" and reply["incident_status"] == "monitoring"
    assert reply["classification"]["label"] == "fine"
    sleep.fire()  # the cancelled timer must not escalate
    await s.idle()
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "monitoring" and "suspend" not in actions.names()
    assert [m["role"] for m in inc["transcript"]] == ["agent", "rider"]
    assert ("ride:ride1", "monitoring") in [(t, d["status"]) for t, d in actions.published if d["type"] == "safety_update"]
    assert "llm_classification" in steps(s, inc["id"])
    assert s.store.checkins[out["checkin_id"]]["llm_classification"]["source"] == "llm"


async def test_distress_escalates():
    actions = FakeActions()
    s, *_ = make_sentinel(actions)
    out = await s.handle_signal(signal(0.46))
    reply = await s.handle_reply(out["checkin_id"], "Help, I'm scared, please stop")
    assert reply["action"] == "escalate" and reply["incident_status"] == "escalated"
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "escalated" and "distress" in inc["reason"]
    assert "suspend" in actions.names()


async def test_unclear_reply_escalates():
    s, *_ = make_sentinel()
    out = await s.handle_signal(signal(0.46))
    reply = await s.handle_reply(out["checkin_id"], "umm what")
    assert reply["action"] == "escalate"
    await settle(s)


async def test_reply_errors():
    s, *_ = make_sentinel()
    with pytest.raises(CheckinNotFound):
        await s.handle_reply("chk_missing", "hi")
    out = await s.handle_signal(signal(0.46))
    await s.handle_reply(out["checkin_id"], "I'm fine")
    with pytest.raises(CheckinClosed) as err:
        await s.handle_reply(out["checkin_id"], "help")
    assert err.value.checkin["status"] == "answered"
    await settle(s)


# ---- the LLM never blocks a decision; it can only upgrade ----

async def test_decision_never_waits_for_a_slow_llm():
    llm = FakeLLM()
    llm.gate = asyncio.Event()  # the LLM hangs until released
    s, rec, *_ = make_sentinel(llm=llm)
    t0 = time.perf_counter()
    out = await asyncio.wait_for(s.handle_signal(signal(0.46)), 1.0)
    reply = await asyncio.wait_for(s.handle_reply(out["checkin_id"], "help"), 1.0)
    assert time.perf_counter() - t0 < 1.0
    assert out["action"] == "check_in" and reply["action"] == "escalate"
    inc = s.store.incidents[out["incident_id"]]
    decision_step = next(e for e in inc["timeline"] if e["step"] == "decision")
    assert inc["reasoning_source"] == "template" and decision_step["details"]["reasoning_source"] == "template"
    template = inc["reasoning"]
    llm.gate.set()  # the LLM answers later...
    await settle(s)
    assert inc["reasoning"] == "LLM says: risky swerve." and inc["reasoning_source"] == "llm"
    assert decision_step["details"]["reasoning"] == "LLM says: risky swerve." and template != inc["reasoning"]
    assert "safety_reasoning" in rec.kinds()


async def test_decision_with_real_llm_client_and_slow_provider():
    async def slow(**_):
        await asyncio.sleep(10)

    llm = make_llm(completion=ScriptedProvider([slow, slow, slow]), timeout_s=0.3, fallback_models="")
    s, rec, *_ = make_sentinel(llm=llm)
    t0 = time.perf_counter()
    out = await s.handle_signal(signal(0.9))
    assert time.perf_counter() - t0 < 0.25 and out["action"] == "escalate"
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["reasoning_source"] == "template (timeout)"
    assert any(e["kind"] == "safety_reasoning" and e["used_fallback"] and e["fallback_reason"] == "timeout"
               for e in rec.events)


async def test_llm_upgrades_a_fine_reply_it_considers_distress():
    actions = FakeActions()
    llm = FakeLLM(assessment={"distress": True, "sentiment": "fearful", "confidence": 0.85})
    s, *_ = make_sentinel(actions, llm)
    out = await s.handle_signal(signal(0.46))
    reply = await s.handle_reply(out["checkin_id"], "yes, fine")  # keywords: fine -> monitor at once
    assert reply["action"] == "monitor"
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "escalated" and "llm_upgrade" in steps(s, inc["id"])
    assert "suspend" in actions.names()


async def test_llm_never_downgrades_distress():
    actions = FakeActions()
    llm = FakeLLM(assessment={"distress": False, "sentiment": "calm", "confidence": 0.99})
    s, *_ = make_sentinel(actions, llm)
    out = await s.handle_signal(signal(0.46))
    await s.handle_reply(out["checkin_id"], "help me")
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "escalated" and "llm_upgrade" not in steps(s, inc["id"])


async def test_llm_fallback_keeps_keyword_result():
    llm = FakeLLM(fallback="rate")
    s, rec, *_ = make_sentinel(llm=llm)
    out = await s.handle_signal(signal(0.46))
    await s.handle_reply(out["checkin_id"], "all good")
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "monitoring" and inc["reasoning_source"] == "template (rate)"
    assert s.store.checkins[out["checkin_id"]]["llm_classification"]["source"] == "keyword fallback (rate)"


# ---- guardrails ----

async def test_rate_limit_three_escalations_per_driver_per_hour():
    actions = FakeActions()
    s, rec, clock, _ = make_sentinel(actions)
    for i in range(3):
        out = await s.handle_signal(signal(0.9, ride=f"ride{i}", signal_id=f"s{i}"))
        assert out["action"] == "escalate"
        clock.advance(60)
    out = await s.handle_signal(signal(0.9, ride="ride9", signal_id="s9"))
    assert out["action"] == "monitor" and out["suppressed"] and out["incident_id"] is None
    assert s.store.suppressed[0]["ride_id"] == "ride9" and "safety_suppressed" in rec.kinds()
    # Another driver is not affected.
    assert (await s.handle_signal(signal(0.9, driver="d_002", ride="rx")))["action"] == "escalate"
    # A timeout escalation over the limit is suppressed too...
    chk = await s.handle_signal(signal(0.5, ride="ride10"))
    await settle(s)
    s._timers[chk["checkin_id"]].cancel()
    s.sleep = ManualSleep()
    s.sleep.fire()
    await s._checkin_timeout(chk["checkin_id"])
    s.sleep = ManualSleep()  # later check-ins wait again
    inc = s.store.incidents[chk["incident_id"]]
    assert inc["status"] == "monitoring" and inc["suppressed"] and "escalation_suppressed" in steps(s, inc["id"])
    # ...but a rider asking for help is never suppressed.
    chk2 = await s.handle_signal(signal(0.5, ride="ride11"))
    assert (await s.handle_reply(chk2["checkin_id"], "help"))["action"] == "escalate"
    # An unclear answer over the limit is suppressed (no distress keyword).
    chk3 = await s.handle_signal(signal(0.5, ride="ride12"))
    r3 = await s.handle_reply(chk3["checkin_id"], "hmm")
    assert r3["action"] == "monitor" and r3["incident_status"] == "monitoring"
    clock.advance(3600)
    assert (await s.handle_signal(signal(0.9, ride="ride13")))["action"] == "escalate"
    await settle(s)


async def test_history_makes_repeat_offenders_escalate():
    s, *_ = make_sentinel()
    for i in range(2):
        out = await s.handle_signal(signal(0.5, ride=f"r{i}"))
        await s.handle_reply(out["checkin_id"], "I'm fine")
    out = await s.handle_signal(signal(0.7, ride="r3"))
    assert out["action"] == "escalate"
    await settle(s)


async def test_resolve_requires_supervisor_confirmation():
    actions = FakeActions()
    rejecting = []

    async def reject(incident, **kw):
        rejecting.append(kw)
        return Confirmation(incident["id"], False, violations=("supervisor offline",))

    s, *_ = make_sentinel(actions, supervisor=reject)
    out = await s.handle_signal(signal(0.9))
    await settle(s)
    with pytest.raises(ResolutionRejected) as err:
        await s.resolve(out["incident_id"], "ops_console", "rider is home safe")
    assert err.value.confirmation.violations == ("supervisor offline",)
    inc = s.store.incidents[out["incident_id"]]
    assert inc["status"] == "escalated" and "unsuspend" not in actions.names()
    assert "supervisor_review" in steps(s, inc["id"])


async def test_safety_agent_cannot_close_or_unfreeze_by_itself():
    actions = FakeActions()
    s, rec, *_ = make_sentinel(actions)  # the real Ops Supervisor
    out = await s.handle_signal(signal(0.9))
    await settle(s)
    iid = out["incident_id"]
    with pytest.raises(ResolutionRejected) as err:
        await s.resolve(iid, "safety_sentinel", "looks fine now")
    assert any("Safety Sentinel" in v for v in err.value.confirmation.violations)
    # The store and the actions refuse anything but an approved confirmation.
    for conf in (None, Confirmation(iid, False), Confirmation(iid, True, agent="safety_sentinel"),
                 Confirmation("other", True)):
        with pytest.raises(ResolutionRefused):
            await s.store.close_incident(iid, conf, "x", "safety_sentinel")
        with pytest.raises(PermissionError):
            await actions.unsuspend_driver("d_001", iid, conf)
    with pytest.raises(ResolutionRefused):
        await s.store.update_incident(iid, status="resolved")
    assert s.store.incidents[iid]["status"] == "escalated"


async def test_supervisor_confirmed_resolution_unfreezes_and_closes():
    actions = FakeActions()
    s, rec, *_ = make_sentinel(actions)
    out = await s.handle_signal(signal(0.9))
    await settle(s)
    snap = await s.resolve(out["incident_id"], "ops_console", "rider picked up safely; driver cleared")
    assert snap["status"] == "resolved" and "confirmed by ops_supervisor" in snap["resolved_by"]
    assert "unsuspend" in actions.names()
    names = steps(s, out["incident_id"])
    assert names[-3:] == ["supervisor_review", "driver_unfrozen", "resolved"]
    sup_events = [e for e in rec.events if e["agent"] == "ops_supervisor"]
    assert sup_events  # the unfreeze and the close are audited under the supervisor
    with pytest.raises(ResolutionRejected):
        await s.resolve(out["incident_id"], "ops_console", "closing again")
    with pytest.raises(IncidentNotFound):
        await s.resolve("inc_missing", "ops", "note text")


async def test_resolve_blocked_while_rider_check_in_pending():
    s, *_ = make_sentinel()
    out = await s.handle_signal(signal(0.5))
    with pytest.raises(ResolutionRejected) as err:
        await s.resolve(out["incident_id"], "ops_console", "nothing happened")
    assert any("check-in" in v for v in err.value.confirmation.violations)
    await s.handle_reply(out["checkin_id"], "I'm fine")
    snap = await s.resolve(out["incident_id"], "ops_console", "rider confirmed safe")
    assert snap["status"] == "resolved"  # no driver to unfreeze on the monitor path
    await settle(s)


# ---- failures never stop the escalation ----

async def test_failed_actions_are_logged_and_the_rest_continue():
    actions = FakeActions(fail={"emergency", "track", "suspend", "contact", "get_ride"})
    s, rec, *_ = make_sentinel(actions)
    out = await s.handle_signal(signal(0.95))
    await settle(s)
    inc = s.store.incidents[out["incident_id"]]
    assert {inc["actions"][k]["status"] for k in ("emergency_notified", "location_shared", "driver_frozen",
                                                   "contact_notified")} == {"failed"}
    names = steps(s, inc["id"])
    for want in ("emergency_dispatch_failed", "location_share_failed", "driver_suspend_failed", "contact_notify_failed"):
        assert want in names
    assert any(e["kind"] == "safety_escalation" and "0/4" in e["decision"] for e in rec.events)


async def test_slow_ride_lookup_times_out_without_blocking():
    actions = FakeActions()
    actions.ride_delay = 5
    s, *_ = make_sentinel(actions, context_timeout_s=0.05)
    t0 = time.perf_counter()
    out = await s.handle_signal(signal(0.46, rider_id=""))
    assert time.perf_counter() - t0 < 1 and out["action"] == "check_in"
    await settle(s)


async def test_escalate_is_idempotent_and_skips_resolved():
    actions = FakeActions()
    s, *_ = make_sentinel(actions)
    out = await s.handle_signal(signal(0.9))
    await settle(s)
    await s._escalate(out["incident_id"], "again")
    assert actions.names().count("suspend") == 1


# ---- views ----

async def test_views_and_detail():
    actions = FakeActions()
    s, rec, clock, sleep = make_sentinel(actions)
    out = await s.handle_signal(signal(0.5))
    await settle(s)
    pend = s.pending_checkins()
    assert pend[0]["checkin_id"] == out["checkin_id"] and pend[0]["remaining_s"] == 15
    clock.advance(5)
    view = s.checkin_view(s.store.checkins[out["checkin_id"]])
    assert view["remaining_s"] == 10 and view["status"] == "pending" and view["incident_status"] == "check_in"
    det = await s.detail(out["incident_id"])
    assert det["vehicle_path"] == actions.path and det["path_source"] == "telemetry"
    assert det["checkins"][0]["id"] == out["checkin_id"] and det["deviation_m"] == 300
    assert det["summary"]["pending_checkin_id"] == out["checkin_id"]
    assert det["planned_route"] and det["timeline"]
    actions.fail.add("path")
    det = await s.detail(out["incident_id"])
    assert det["path_source"] == "signal_trail" and det["vehicle_path"][0]["lat"] == 12.975
    with pytest.raises(IncidentNotFound):
        await s.detail("nope")
    summ = s.summary(s.store.incidents[out["incident_id"]])
    assert summ["flags"] == {"driver_frozen": False, "emergency_notified": False, "location_shared": False,
                             "contact_notified": False}
    # A check-in whose incident is gone still renders.
    s.store.checkins["chk_orphan"] = {**s.store.checkins[out["checkin_id"]], "id": "chk_orphan",
                                      "incident_id": "inc_gone"}
    assert s.checkin_view(s.store.checkins["chk_orphan"])["incident_status"] is None
    assert any(p.get("id") == "chk_orphan" for p in s.pending_checkins())


def test_emergency_contact_is_deterministic():
    a, b = emergency_contact("rider-1"), emergency_contact("rider-1")
    assert a == b and a["phone"].startswith("+91 90000 ") and "rider-1" in a["name"]
    assert "the rider" in emergency_contact("")["name"]


async def test_spawned_task_failures_are_logged_not_raised(caplog):
    s, *_ = make_sentinel()

    async def boom():
        raise RuntimeError("kaput")

    s.spawn(boom(), "boom")
    await s.idle()
    assert "boom task failed" in caplog.text


async def test_record_failure_never_breaks_a_decision():
    async def broken(*a, **k):
        raise RuntimeError("db down")

    s, *_ = make_sentinel(record=broken)
    out = await s.handle_signal(signal(0.9))
    assert out["action"] == "escalate"
    await settle(s)


async def test_reasoning_for_vanished_incident_is_dropped():
    llm = FakeLLM()
    llm.gate = asyncio.Event()
    s, *_ = make_sentinel(llm=llm)
    out = await s.handle_signal(signal(0.9))
    del s.store.incidents[out["incident_id"]]
    llm.gate.set()
    await settle(s)
