"""Booking Intent graph: golden utterances through a mocked LLM and the fallback, questions, confirm, audit."""
import asyncio
import json

import pytest

from src.agents_py.agents.booking.gateway import GatewayError
from src.agents_py.agents.booking.planner import (
    PlanNotFound, QuestionNotFound, build_prompt, get_booking_agent, model_label, reset_booking_agent,
)
from src.agents_py.agents.booking.places import PERSONAS
from src.agents_py.agents.pricing.quote import CellLoad
from tests.booking_fakes import GOLDEN, GOLDEN_SENTENCE, NOW, FakeGateway, make_agent, plan_summary
from tests.helpers import ScriptedProvider, llm_response, make_llm


def llm_for(answer, **kw):
    return make_llm(completion=ScriptedProvider([llm_response(answer, tokens=420)]), **kw)


@pytest.mark.parametrize("i", range(len(GOLDEN)))
async def test_golden_with_mocked_llm(i):
    text, expected, answer = GOLDEN[i]
    agent, rec = make_agent(llm=llm_for(answer))
    plan = await agent.parse("rider_priya", text)
    assert plan["type"] == "plan", plan
    assert plan_summary(plan) == expected
    assert plan["answered_by"]["used_fallback"] is False
    assert plan["answered_by"]["model"] == "gemini/gemini-2.0-flash"
    assert plan["answered_by"]["label"] == "gemini-2.0-flash" and plan["answered_by"]["tokens"] == 420
    ev = rec.events[-1]
    assert ev["agent"] == "Booking Intent" and ev["used_fallback"] is False
    assert ev["metadata"]["model"] == "gemini/gemini-2.0-flash" and ev["metadata"]["kind"] == "booking_plan"


@pytest.mark.parametrize("i", range(len(GOLDEN)))
async def test_golden_through_fallback_when_llm_disabled(i):
    text, expected, _ = GOLDEN[i]
    agent, rec = make_agent(llm=make_llm(enabled=False))
    plan = await agent.parse("rider_priya", text)
    assert plan_summary(plan) == expected
    assert plan["answered_by"]["used_fallback"] is True
    assert plan["answered_by"]["fallback_reason"] == "disabled"
    assert plan["answered_by"]["label"] == "rule-based"
    assert rec.events[-1]["used_fallback"] is True and rec.events[-1]["metadata"]["model"] == "rule-based"


async def test_golden_sentence_plan_details():
    agent, _ = make_agent(llm=llm_for(GOLDEN[0][2]))
    plan = await agent.parse("rider_priya", GOLDEN_SENTENCE)
    kid, me = plan["legs"]
    assert (kid["seq"], kid["passenger_name"], kid["pickup_time"]) == (1, "Anaya", "3:15 PM")
    assert kid["origin"]["label"].startswith("Anaya's school") and kid["destination"]["label"].startswith("Soccer")
    assert (me["seq"], me["passenger_name"], me["tier"], me["arrive_by_time"]) == (2, "Priya", "premium", "6:00 PM")
    # office -> airport worked backwards: pickup = 18:00 - trip - 10 min buffer
    assert me["eta_source"] == "haversine" and 45 < me["duration_min"] < 52
    assert me["pickup_time"] in ("4:59 PM", "5:00 PM", "5:01 PM", "5:02 PM")
    assert me["price"]["tier"] == "premium" and me["price"]["surge_multiplier"] == 1.0
    assert plan["total_fare"] == kid["price"]["total"] + me["price"]["total"]
    assert plan["warnings"] == [] and plan["currency"] == "INR"
    assert plan["plan_id"].startswith("pln_") and plan["plan_id"] in agent.plans


async def test_llm_timeout_falls_back_and_surge_and_eta_from_dispatch():
    async def slow(**kw):
        await asyncio.sleep(1)

    llm = make_llm(completion=ScriptedProvider([slow, slow]), timeout_s=0.01)
    gw = FakeGateway(eta_fails=False, load=CellLoad("c1", supply=2, demand=8, source="dispatch-go"))
    agent, rec = make_agent(llm=llm, gateway=gw)
    plan = await agent.parse("rider_priya", "Book a sedan from home to the office at 9:30 AM")
    assert plan["answered_by"]["fallback_reason"] == "timeout"
    leg = plan["legs"][0]
    assert leg["eta_source"] == "dispatch-go" and leg["duration_min"] == 30.0
    assert leg["price"]["surge_multiplier"] == 2.5  # 8 busy / 2 idle -> 2.5x (cap)
    assert rec.events[-1]["fallback_reason"] == "timeout"


async def test_force_fallback_skips_the_llm():
    provider = ScriptedProvider([llm_response(GOLDEN[0][2])])
    agent, rec = make_agent(llm=make_llm(completion=provider))
    plan = await agent.parse("rider_priya", GOLDEN_SENTENCE, force_fallback=True)
    assert provider.calls == []
    assert plan["answered_by"] == {**plan["answered_by"], "used_fallback": True, "fallback_reason": "forced",
                                   "label": "rule-based"}
    assert plan_summary(plan) == GOLDEN[0][1]
    assert rec.events[-1]["fallback_reason"] == "forced"


async def test_prompt_mentions_persona_and_time():
    prompt = build_prompt("hi", PERSONAS["rider_priya"], NOW)
    assert "Priya Sharma" in prompt and "Wednesday 2026-11-18 08:00 IST" in prompt and "Anaya" in prompt
    assert "ONE leg" in prompt
    assert model_label("gemini/gemini-3.5-flash-lite") == "gemini-3.5-flash-lite"
    assert model_label("deterministic-fallback") == "rule-based"


# ---- ambiguity -> question ----

async def test_which_school_question_and_answer():
    agent, rec = make_agent(llm=make_llm(enabled=False))
    q = await agent.parse("rider_arjun", "Pick up my kid from school at 3:15 PM and drop them at home")
    assert q["type"] == "question" and q["field"] == "place:0:origin"
    assert q["question"] == "Which school - Aanya's school or Kabir's school?"
    assert q["options"] == ["Aanya's school", "Kabir's school"]
    assert rec.events[-1]["metadata"]["kind"] == "booking_question"
    plan = await agent.parse("rider_arjun", "Kabir's school", reply_to=q["question_id"])
    assert plan["type"] == "plan"
    assert plan["legs"][0]["origin"]["key"] == "school_kabir" and plan["legs"][0]["pickup_time"] == "3:15 PM"
    assert plan["legs"][0]["tier"] == "sedan"  # Arjun's default
    assert rec.events[-1]["metadata"]["reply_to"] == q["question_id"]
    with pytest.raises(QuestionNotFound):
        await agent.parse("rider_arjun", "Kabir", reply_to=q["question_id"])


async def test_school_named_kid_needs_no_question():
    agent, _ = make_agent(llm=make_llm(enabled=False))
    plan = await agent.parse("rider_arjun", "Pick up Aanya from school at 3:15 PM and drop her at home")
    assert plan["type"] == "plan" and plan["legs"][0]["origin"]["key"] == "school_aanya"


async def test_place_answer_by_partial_name():
    agent, _ = make_agent(llm=make_llm(enabled=False))
    q = await agent.parse("rider_arjun", "from school to home at 4 pm")
    plan = await agent.parse("rider_arjun", "the aanya one", reply_to=q["question_id"])
    assert plan["legs"][0]["origin"]["key"] == "school_aanya"


async def test_missing_time_for_kid_question():
    agent, _ = make_agent(llm=make_llm(enabled=False))
    q = await agent.parse("rider_priya", "Pick up my kid from school and drop them at soccer practice")
    assert q["type"] == "question" and q["field"] == "time:0"
    assert q["question"] == "What time should we pick up Anaya from school?"
    assert q["options"] == ["Now", "8:30 AM", "9:00 AM"]
    plan = await agent.parse("rider_priya", "3:45 PM", reply_to=q["question_id"])
    assert plan["legs"][0]["pickup_time"] == "3:45 PM"
    q2 = await agent.parse("rider_priya", "Pick up my kid from school and drop them at soccer practice")
    plan = await agent.parse("rider_priya", "now", reply_to=q2["question_id"])
    assert plan["legs"][0]["pickup_time"] == "8:05 AM"
    q3 = await agent.parse("rider_priya", "Pick up my kid from school and drop them at soccer practice")
    again = await agent.parse("rider_priya", "whenever", reply_to=q3["question_id"])
    assert again["type"] == "question" and again["field"] == "time:0"


async def test_missing_destination_and_nothing_understood():
    agent, _ = make_agent(llm=make_llm(enabled=False))
    q = await agent.parse("rider_priya", "pick up my mom from the airport at 9 pm")
    assert q["field"] == "destination:0" and q["question"] == "Where should my mom be dropped off?"
    assert "Office" in q["options"] and q["options"][-1] == "Home"
    plan = await agent.parse("rider_priya", "home", reply_to=q["question_id"])
    assert plan["legs"][0]["destination"]["key"] == "home" and plan["legs"][0]["passenger"] == "mom"
    q = await agent.parse("rider_priya", "hello there")
    assert q["field"] == "text" and q["options"] == ["Home", "Office", "Airport"]
    plan = await agent.parse("rider_priya", "to the airport now", reply_to=q["question_id"])
    assert plan["type"] == "plan" and plan["legs"][0]["destination"]["key"] == "airport"


async def test_unknown_place_question_then_answer():
    agent, _ = make_agent(llm=make_llm(enabled=False))
    q = await agent.parse("rider_priya", "take me to Narnia Palace at 5 pm")
    assert q["field"] == "placetext:0:destination" and '"Narnia Palace"' in q["question"]
    plan = await agent.parse("rider_priya", "Koramangala", reply_to=q["question_id"])
    assert plan["legs"][0]["destination"]["label"] == "Koramangala"


async def test_llm_with_no_legs_asks():
    agent, _ = make_agent(llm=llm_for({"legs": [], "ambiguities": ["unclear"]}))
    q = await agent.parse("rider_priya", "umm")
    assert q["type"] == "question" and q["answered_by"]["used_fallback"] is False


# ---- feasibility ----

async def test_feasibility_warnings():
    answer = {"legs": [
        {"passenger": "kid", "origin": "home", "destination": "home", "pickup_at": "10:00", "tier": "pool"},
        {"passenger": "me", "origin": "office", "destination": "Mysore", "pickup_at": "11:00", "tier": "jet"}]}
    far = {"mysore": ("Mysuru", 12.2958, 76.6394)}
    from tests.booking_fakes import FakeGeocoder

    agent, _ = make_agent(llm=llm_for(answer), geocoder=FakeGeocoder(far))
    plan = await agent.parse("rider_priya", "x")
    kid, me = plan["legs"]
    assert kid["tier"] == "mini" and "Children can't ride Pool alone; booked a Mini instead." in kid["warnings"]
    assert any("same place" in w for w in kid["warnings"])
    assert me["tier"] == "mini" and 'Unknown car type "jet"; using mini.' in me["warnings"]
    assert "Mysuru is outside the Bengaluru service area." in me["warnings"]


# ---- confirm ----

async def test_confirm_schedules_legs_once():
    gw = FakeGateway()
    agent, rec = make_agent(llm=make_llm(enabled=False), gateway=gw)
    plan = await agent.parse("rider_priya", GOLDEN_SENTENCE)
    out = await agent.confirm(plan["plan_id"])
    assert out["status"] == "scheduled" and out["scheduled_ride_ids"] == ["sch_1", "sch_2"]
    sent = gw.scheduled[0]
    assert sent["rider_id"] == "rider_priya" and [lg["passenger"] for lg in sent["legs"]] == ["kid", "me"]
    assert sent["legs"][1]["tier"] == "premium" and sent["legs"][0]["pickup_at"].startswith("2026-11-18T15:15")
    assert json.dumps(sent)  # serialisable
    assert rec.events[-1]["metadata"]["kind"] == "booking_confirm" and rec.events[-1]["used_fallback"] is True
    again = await agent.confirm(plan["plan_id"])
    assert again is out and len(gw.scheduled) == 1
    with pytest.raises(PlanNotFound):
        await agent.confirm("pln_nope")


async def test_confirm_propagates_gateway_errors():
    agent, _ = make_agent(llm=make_llm(enabled=False), gateway=FakeGateway(schedule_error=GatewayError("down")))
    plan = await agent.parse("rider_priya", "from home to the office at 9:30 am")
    with pytest.raises(GatewayError):
        await agent.confirm(plan["plan_id"])
    assert plan["plan_id"] not in agent.confirmed


async def test_audit_failures_never_break_booking():
    async def broken(*a, **k):
        raise RuntimeError("db gone")

    agent, _ = make_agent(llm=make_llm(enabled=False))
    agent.record = broken
    plan = await agent.parse("rider_priya", "from home to the office at 9:30 am")
    assert (await agent.confirm(plan["plan_id"]))["status"] == "scheduled"


async def test_plans_and_drafts_are_bounded(monkeypatch):
    from src.agents_py.agents.booking import planner

    monkeypatch.setattr(planner, "MAX_KEPT", 2)
    agent, _ = make_agent(llm=make_llm(enabled=False))
    for _ in range(3):
        await agent.parse("rider_priya", "from home to the office at 9:30 am")
    assert len(agent.plans) == 2


def test_singleton():
    reset_booking_agent()
    a = get_booking_agent()
    assert get_booking_agent() is a
    reset_booking_agent()
    assert get_booking_agent() is not a
    reset_booking_agent()
