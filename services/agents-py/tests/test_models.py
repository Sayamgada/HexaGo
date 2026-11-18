"""ParsedRequest / LegSpec normalisation of LLM output."""
import pytest
from pydantic import ValidationError

from src.agents_py.agents.booking.models import LegSpec, ParsedRequest


def test_leg_normalisation():
    lg = LegSpec(passenger="Daughter", pickup_at="9:05", arrive_by="null", tier=" Premium ")
    assert (lg.passenger, lg.pickup_at, lg.arrive_by, lg.tier) == ("kid", "09:05", None, "premium")
    assert LegSpec(passenger=None, tier="", pickup_at="").passenger == "me"
    assert LegSpec(tier=None).tier is None and LegSpec(tier="").tier is None
    assert LegSpec(passenger="Mom").passenger == "mom"


def test_bad_times_are_rejected():
    with pytest.raises(ValidationError):
        LegSpec(pickup_at="3:15 PM")
    with pytest.raises(ValidationError):
        LegSpec(arrive_by="24:00")


def test_parsed_request_defaults():
    p = ParsedRequest()
    assert p.legs == [] and p.tier is None and p.ambiguities == []
