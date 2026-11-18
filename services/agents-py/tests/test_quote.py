"""Pricing quote: per-tier fares, minimum fares, surge ratio and its 2.5x cap."""
import pytest

from src.agents_py.agents.pricing.quote import (
    RATES, SURGE_CAP, TIERS, CellLoad, RatioSurge, normalize_tier, quote, ratio_surge,
)


@pytest.mark.parametrize("tier,expected", [
    # base + per_km * 10 + per_min * 30
    ("auto", 30 + 150 + 30),
    ("mini", 50 + 180 + 45),
    ("sedan", 70 + 220 + 60),
    ("premium", 120 + 300 + 90),
    ("xl", 100 + 280 + 75),
    ("av", 80 + 200 + 45),
    ("pool", 40 + 120 + 30),
])
def test_quote_per_tier_without_surge(tier, expected):
    q = quote(tier, 10, 30)
    assert q["total"] == expected
    assert q["subtotal"] == expected
    assert q["surge_multiplier"] == 1.0 and q["surge_amount"] == 0
    assert q["currency"] == "INR" and q["tier"] == tier
    assert q["base"] == RATES[tier].base
    assert q["distance_fare"] == RATES[tier].per_km * 10
    assert q["time_fare"] == RATES[tier].per_min * 30
    assert q["minimum_applied"] is False


def test_tiers_are_ordered_by_price():
    prices = {t: quote(t, 10, 30)["total"] for t in TIERS}
    assert prices["pool"] < prices["mini"] < prices["sedan"] < prices["premium"]
    assert prices["auto"] < prices["mini"]
    assert set(TIERS) == {"auto", "mini", "sedan", "premium", "xl", "av", "pool"}


def test_minimum_fare_applies_to_short_trips():
    q = quote("premium", 0.5, 2)
    assert q["subtotal"] == RATES["premium"].minimum
    assert q["minimum_applied"] is True
    assert q["total"] == 200


@pytest.mark.parametrize("supply,demand,mult", [
    (10, 0, 1.0),     # no demand
    (10, 5, 1.0),     # oversupplied
    (10, 10, 1.0),    # balanced
    (10, 20, 1.5),    # 2:1
    (10, 30, 2.0),    # 3:1
    (10, 40, 2.5),    # 4:1 hits the cap
    (2, 100, 2.5),    # 50:1 capped
    (0, 3, 2.5),      # nobody idle
    (0, 0, 1.0),      # empty cell
])
def test_ratio_surge(supply, demand, mult):
    assert ratio_surge(CellLoad(supply=supply, demand=demand)) == mult


def test_surge_is_capped_at_2_5x():
    assert SURGE_CAP == 2.5
    for demand in range(0, 500, 7):
        assert 1.0 <= ratio_surge(CellLoad(supply=3, demand=demand)) <= SURGE_CAP


def test_quote_applies_surge_and_reports_breakdown():
    load = CellLoad(cell="8961892", supply=4, demand=10, source="dispatch-go")
    q = quote("sedan", 10, 30, load)
    assert q["surge_multiplier"] == 1.75
    assert q["total"] == round(350 * 1.75)
    assert q["surge_amount"] == q["total"] - 350
    assert q["cell"] == {"id": "8961892", "supply": 4, "demand": 10, "source": "dispatch-go"}
    assert q["surge_model"] == "supply_demand_ratio"


def test_surge_model_is_pluggable():
    class Flat:
        name = "flat_2x"

        def multiplier(self, load):
            return 2.0

    q = quote("mini", 10, 30, surge=Flat())
    assert q["surge_multiplier"] == 2.0 and q["total"] == 550 and q["surge_model"] == "flat_2x"
    assert RatioSurge(cap=1.2).multiplier(CellLoad(supply=1, demand=50)) == 1.2


def test_tier_validation():
    assert normalize_tier(" Sedan ") == "sedan"
    with pytest.raises(ValueError):
        normalize_tier("helicopter")
    with pytest.raises(ValueError):
        quote("mini", -1, 3)
    with pytest.raises(ValueError):
        quote("mini", 1, -3)
