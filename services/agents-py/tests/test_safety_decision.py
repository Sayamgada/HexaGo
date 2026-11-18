"""Pure Safety Sentinel rules: thresholds, reply classifier, rate limit, supervisor rule-check."""
import pytest

from src.agents_py.agents.safety import decision as d
from src.agents_py.agents.safety.decision import (
    CHECK_IN, DISTRESS, ESCALATE, FINE, MONITOR, UNCLEAR, Confirmation, ReplyClass, SignalContext,
)


@pytest.mark.parametrize("sev,action", [
    (0.0, MONITOR), (0.29, MONITOR), (0.3, CHECK_IN), (0.46, CHECK_IN), (0.79, CHECK_IN),
    (0.8, ESCALATE), (0.95, ESCALATE), (1.5, ESCALATE), (-1, MONITOR),
])
def test_decide_thresholds(sev, action):
    assert d.decide(sev).action == action


def test_severity_at_or_above_point_eight_escalates_immediately_regardless_of_context():
    out = d.decide(0.8, SignalContext(prior_incidents=0, tier="av"))
    assert out.action == ESCALATE and "escalating immediately" in out.reason and out.signal_severity == 0.8


def test_driver_history_raises_concern():
    assert d.effective_severity(0.7, SignalContext(prior_incidents=2)) == 0.8
    assert d.effective_severity(0.7, SignalContext(prior_incidents=10)) == 0.85  # capped at 3 incidents
    out = d.decide(0.7, SignalContext(prior_incidents=2))
    assert out.action == ESCALATE and "prior incident" in out.reason
    assert d.decide(0.27, SignalContext(prior_incidents=1)).action == CHECK_IN


@pytest.mark.parametrize("text,label", [
    ("I'm fine", FINE), ("im fine thanks", FINE), ("All good!", FINE), ("yes I'm okay", FINE),
    ("it was just a pothole", FINE), ("Yeah, false alarm", FINE), ("I am safe", FINE),
    ("help", DISTRESS), ("HELP ME", DISTRESS), ("I'm scared", DISTRESS), ("please stop the car", DISTRESS),
    ("call the police", DISTRESS), ("I feel unsafe", DISTRESS), ("I'm hurt", DISTRESS),
    ("I'm not ok", DISTRESS), ("not okay", DISTRESS), ("I don’t feel safe", DISTRESS),
    ("I'm fine but he has a knife", DISTRESS), ("no", DISTRESS), ("nooo", DISTRESS),
    ("", UNCLEAR), (None, UNCLEAR), ("hmm what", UNCLEAR), ("banana", UNCLEAR),
])
def test_keyword_classifier(text, label):
    assert d.classify_reply(text).label == label


def test_classifier_reports_matches_and_confidence():
    c = d.classify_reply("Help! I'm scared")
    assert c.matched == ("help", "scared") and c.confidence == 0.9
    assert d.classify_reply("I'm fine").confidence == 0.8
    assert d.classify_reply("???").confidence == 0.0


def test_decide_after_reply():
    assert d.decide_after_reply(d.classify_reply("I'm fine"), 0.5).action == MONITOR
    distress = d.decide_after_reply(d.classify_reply("help"), 0.5)
    assert distress.action == ESCALATE and distress.severity == 0.8 and "distress" in distress.reason
    assert d.decide_after_reply(ReplyClass(UNCLEAR), 0.4).action == ESCALATE


def test_timeout_escalates():
    out = d.decide_on_timeout(15, 0.4)
    assert out.action == ESCALATE and "15 s" in out.reason and out.severity == 0.8
    assert d.checkin_timeout_s(True) == 15 and d.checkin_timeout_s(False) == 30


@pytest.mark.parametrize("current,llm,want", [
    (MONITOR, {"distress": True, "confidence": 0.9}, ESCALATE),
    (CHECK_IN, {"distress": True, "confidence": 0.6}, ESCALATE),
    (MONITOR, {"distress": True, "confidence": 0.5}, None),   # not confident enough
    (MONITOR, {"distress": False, "confidence": 0.99}, None),  # never downgrades or acts on "fine"
    (ESCALATE, {"distress": False, "confidence": 1}, None),    # cannot downgrade an escalation
    (ESCALATE, {"distress": True, "confidence": 1}, None),
    (MONITOR, {"distress": True, "confidence": "x"}, None),
    (MONITOR, None, None),
])
def test_llm_can_only_upgrade(current, llm, want):
    assert d.upgrade_action(current, llm) == want


def test_rate_limit_three_per_driver_per_hour():
    now = 10_000.0
    assert d.escalation_allowed([], now)
    assert d.escalation_allowed([now - 10, now - 20], now)
    assert not d.escalation_allowed([now - 10, now - 20, now - 3599], now)
    assert d.escalation_allowed([now - 10, now - 20, now - 3600], now)  # the oldest left the window
    assert d.escalation_allowed([now + 5] * 5, now)  # future stamps do not count


def test_apply_rate_limit_suppresses_automatic_escalations_only():
    times = [100.0, 200.0, 300.0]
    esc = d.decide(0.9)
    out = d.apply_rate_limit(esc, times, 400.0)
    assert out.action == MONITOR and out.suppressed and "false positive" in out.reason
    # A rider who asked for help is never suppressed.
    assert d.apply_rate_limit(esc, times, 400.0, rider_confirmed=True) is esc
    # Under the limit, or not an escalation: unchanged.
    assert d.apply_rate_limit(esc, times[:2], 400.0) is esc
    chk = d.decide(0.5)
    assert d.apply_rate_limit(chk, times, 400.0) is chk


def test_wording():
    trig = [{"type": "route_deviation"}, {"type": "harsh_deceleration"}, {"type": "route_deviation"},
            {"type": "unexpected_stop"}, {"type": "mystery"}]
    assert d.describe_triggers(trig) == "left the planned route, braked very hard and stopped unexpectedly"
    assert d.describe_triggers([{"type": "speed_anomaly"}]) == "was driving unusually fast"
    assert d.describe_triggers([]) == "reported an anomaly"
    assert "Are you okay?" in d.checkin_question(trig)
    sig = {"triggers": trig}
    t = d.template_reasoning(sig, d.decide(0.5), SignalContext(prior_incidents=2))
    assert "2 earlier incident" in t and "call the rider" in t
    assert "escalate" in d.template_reasoning(sig, d.decide(0.9))
    assert "keep monitoring" in d.template_reasoning(sig, d.decide(0.1))
    sup = d.apply_rate_limit(d.decide(0.9), [1.0, 2.0, 3.0], 4.0)
    assert "rate limit" in d.template_reasoning(sig, sup)
    assert d.deviation_threshold_m() == 300
    assert d.normalize("  Hello,‘World’!  ") == "hello 'world'"


def test_resolution_rule_check():
    inc = {"id": "inc_1", "status": "escalated"}
    assert d.resolution_violations(inc, "ops_console", "rider picked up by family") == []
    assert any("Safety Sentinel" in v for v in d.resolution_violations(inc, "safety_sentinel", "closing it"))
    assert "requested_by is required" in d.resolution_violations(inc, " ", "closing it")
    assert any("already resolved" in v for v in d.resolution_violations({"status": "resolved"}, "ops", "again!"))
    assert any("check-in" in v for v in d.resolution_violations(inc, "ops", "closing", pending_checkins=1))
    assert any("note" in v for v in d.resolution_violations(inc, "ops", "ok"))


def test_confirmation_validity():
    assert Confirmation("i", True).valid
    assert not Confirmation("i", False).valid
    assert not Confirmation("i", True, agent="safety_sentinel").valid
    assert not Confirmation("i", True, violations=("x",)).valid
