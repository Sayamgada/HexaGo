"""Rule-based booking parser: the 10 golden utterances and the common patterns."""
import pytest

from src.agents_py.agents.booking.fallback_parser import (
    clean_place, find_tier, parse, parse_answer_time, parse_clock, split_clauses,
)
from tests.booking_fakes import GOLDEN, GOLDEN_SENTENCE, parsed_summary

# What the parser itself extracts (place words before resolution) for each golden utterance.
RAW = {
    0: [("kid", "school", "soccer practice", "15:15", None, None, 0),
        ("me", "office", "airport", None, "18:00", "premium", 0)],
    1: [("me", "home", "office", "09:30", None, None, 0)],
    2: [("me", None, "airport", None, "18:00", "xl", 0)],
    3: [("me", "office", "home", "19:15", None, "auto", 0)],
    4: [("kid", "home", "soccer practice", "16:00", None, "premium", 0)],
    5: [("me", "home", "MG Road", "08:45", None, "pool", 0)],
    6: [("me", None, "airport", "05:30", None, "av", 1)],
    7: [("me", "office", "home", "18:30", None, None, 0), ("me", "home", "airport", None, "22:00", None, 0)],
    8: [("kid", "school", "home", "15:30", None, None, 0)],
    9: [("me", "Koramangala", "Indiranagar", "11:00", None, "sedan", 0)],
}


@pytest.mark.parametrize("i", range(len(GOLDEN)))
def test_golden_utterances(i):
    text = GOLDEN[i][0]
    out = parse(text)
    assert parsed_summary(out) == RAW[i], text
    assert out.ambiguities == []


def test_golden_sentence_passengers_and_labels():
    out = parse(GOLDEN_SENTENCE)
    kid, me = out.legs
    assert kid.passenger_label == "my kid" and kid.asap is False
    assert me.passenger == "me" and me.tier == "premium"


@pytest.mark.parametrize("raw,expected", [
    ("3:15 PM", "15:15"), ("3:15pm", "15:15"), ("3:15 p.m.", "15:15"), ("6", "18:00"), ("6:00", "18:00"),
    ("8", "08:00"), ("11", "11:00"), ("12", "12:00"), ("15:15", "15:15"), ("12 am", "00:00"), ("12 pm", "12:00"),
    ("9:30 AM", "09:30"), ("noon", "12:00"), ("midnight", "00:00"), ("7.45 pm", "19:45"), ("0:30", "00:30"),
    ("13 pm", None), ("25:00", None), ("9:75", None), ("abc", None), ("0 am", None),
])
def test_parse_clock(raw, expected):
    assert parse_clock(raw) == expected


def test_split_clauses():
    assert split_clauses(GOLDEN_SENTENCE) == [
        "Pick up my kid from school at 3:15 PM", "drop them at soccer practice",
        "book a high-tier sedan for me from my office to the airport to arrive by 6:00 PM"]
    assert split_clauses("from A to B; from B to C") == ["from A to B", "from B to C"]
    assert split_clauses("Go home. Then go to work") == ["Go home", "go to work"]


@pytest.mark.parametrize("text,tier", [
    ("a high-tier sedan", "premium"), ("luxury car", "premium"), ("an SUV", "xl"), ("6-seater", "xl"),
    ("a robotaxi", "av"), ("driverless", "av"), ("shared ride", "pool"), ("auto rickshaw", "auto"),
    ("a sedan", "sedan"), ("cheapest", "mini"), ("hatchback", "mini"), ("a cab", None),
    ("autonomous", "av"),
])
def test_find_tier(text, tier):
    assert find_tier(text)[0] == tier


def test_asap_relative_and_missing_destination():
    out = parse("I need a cab to MG Road right now")
    assert out.legs[0].asap is True and out.legs[0].destination == "MG Road" and out.legs[0].pickup_at is None
    assert parse("in 20 minutes to the office please").legs[0].asap is True
    assert parse("in an hour, to the office").legs[0].destination == "office"
    none = parse("hello there")
    assert none.legs == [] and none.ambiguities == ["no destination found"]
    nodest = parse("pick up my mom from the airport at 9 pm")
    assert nodest.legs[0].passenger == "mom" and nodest.legs[0].destination is None
    assert nodest.ambiguities == ["leg 1: no destination"]


def test_drop_starts_a_new_leg_from_the_last_destination():
    out = parse("Take my son from home to school at 8 am, then drop him at football at 4 pm")
    assert [(lg.origin, lg.destination, lg.pickup_at, lg.passenger) for lg in out.legs] == [
        ("home", "school", "08:00", "kid"), ("school", "football", "16:00", "kid")]
    alone = parse("drop me at the airport")
    assert alone.legs[0].origin is None and alone.legs[0].destination == "airport"


def test_pickup_with_destination_in_same_clause_and_relations():
    out = parse("Pick up my mum from Majestic to home at 7:30 pm")
    lg = out.legs[0]
    assert (lg.passenger, lg.origin, lg.destination, lg.pickup_at) == ("mom", "Majestic", "home", "19:30")
    me = parse("pick me up from the office at 6")
    assert me.legs[0].passenger == "me" and me.legs[0].pickup_at == "18:00"


def test_tier_only_clause_applies_to_previous_leg():
    out = parse("from home to the office at 9 am, make it a sedan")
    assert out.legs[0].tier == "sedan"
    out = parse("an SUV please. from home to the airport by 5")
    assert out.tier == "xl" and out.legs[0].arrive_by == "17:00"


def test_from_only_and_reach_and_tomorrow():
    assert parse("from the airport at 11 pm").legs[0].origin == "airport"
    out = parse("reach the office by 9:15 tomorrow")
    assert out.legs[0].destination == "office" and out.legs[0].arrive_by == "09:15" and out.legs[0].day_offset == 1
    assert parse("to the airport this evening at 7").legs[0].pickup_at == "19:00"


def test_clean_place():
    assert clean_place(None) is None
    assert clean_place("  the airport please ") == "airport"
    assert clean_place("reach my office") == "office"
    assert clean_place("MG Road at") == "MG Road"
    assert clean_place(" , ") is None


@pytest.mark.parametrize("answer,expected", [
    ("3:30 PM", ("15:30", False)), ("at 4", ("16:00", False)), ("now", (None, True)),
    ("in 30 minutes", ("08:30", False)), ("In 1 hour", ("09:00", False)), ("in half an hour", ("08:30", False)),
    ("5", ("17:00", False)), ("by 6 pm", ("18:00", False)), ("no idea", (None, False)),
])
def test_parse_answer_time(answer, expected):
    assert parse_answer_time(answer, 8 * 60) == expected
