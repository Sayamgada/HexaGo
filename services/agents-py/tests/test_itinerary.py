"""Itinerary timing: backward ETA from arrive-by, ordering, rollover and conflict warnings."""
import datetime as dt

import pytest

from src.agents_py.agents.booking.itinerary import (
    ARRIVE_BUFFER_MIN, IST, LegTiming, TripEstimate, duration_for_road_km, estimate, fmt_time,
    haversine_estimate, plan_times, short,
)

NOW = dt.datetime(2026, 11, 18, 8, 0, tzinfo=IST)
OFFICE = (13.0475, 77.6200)
AIRPORT = (13.1989, 77.7068)


def lt(passenger="me", minutes=30.0, pickup=None, arrive=None, day=0, asap=False, o="Office - Manyata", d="Airport"):
    return LegTiming(passenger, o, d, TripEstimate(10.0, minutes, "haversine"), pickup, arrive, day, asap)


def hm(t):
    return t.astimezone(IST).strftime("%H:%M")


def test_speed_model():
    assert duration_for_road_km(0.1) == 3.0  # minimum trip
    assert duration_for_road_km(3.0) == 10.0  # 18 km/h in the city
    assert duration_for_road_km(6.0) == 20.0
    assert duration_for_road_km(26.0) == 50.0  # then 40 km/h
    est = haversine_estimate(OFFICE, AIRPORT)
    assert est.source == "haversine"
    assert 24 < est.distance_km < 26 and 45 < est.duration_min < 52


async def test_estimate_prefers_dispatch_and_falls_back():
    async def ok(a, b):
        return TripEstimate(30.0, 55.0, "dispatch-go")

    async def down(a, b):
        raise ConnectionError("no")

    assert (await estimate(ok, OFFICE, AIRPORT)).source == "dispatch-go"
    assert (await estimate(down, OFFICE, AIRPORT)).source == "haversine"
    assert (await estimate(None, OFFICE, AIRPORT)).source == "haversine"


def test_arrive_by_works_backwards_with_buffer():
    (t,), warnings = plan_times([lt(minutes=55, arrive="18:00")], NOW)
    assert hm(t.pickup) == "16:55"  # 18:00 - 55 min - 10 min buffer
    assert ARRIVE_BUFFER_MIN == 10
    assert hm(t.arrival) == "17:50" and hm(t.arrive_by) == "18:00"
    assert warnings == [] and t.warnings == []


def test_pickup_at_and_order_by_pickup():
    timed, _ = plan_times([lt(arrive="18:00", minutes=50), lt("kid", 20, pickup="15:15", o="School", d="Soccer")],
                          NOW)
    assert [t.index for t in timed] == [1, 0]
    assert hm(timed[0].pickup) == "15:15" and hm(timed[0].arrival) == "15:35"
    assert timed[0].arrive_by is None


def test_overlap_warning_for_the_same_passenger():
    legs = [lt(minutes=40, pickup="16:30", o="Soccer - Stadium", d="Office - Manyata"),
            lt(minutes=55, arrive="18:00", o="Office - Manyata", d="Airport (BLR)")]
    timed, warnings = plan_times(legs, NOW)
    # soccer -> office ends 17:10; office -> airport leaves 16:55 (18:00 - 55 - 10)
    assert hm(timed[0].pickup) == "16:30" and hm(timed[1].pickup) == "16:55"
    assert warnings == ["Soccer -> Office ends 5:10 PM, Office -> Airport (BLR) needs 55 min and leaves at "
                        "4:55 PM - overlap."]
    assert timed[1].warnings == warnings


def test_tight_connection_and_other_passengers_do_not_conflict():
    legs = [lt(minutes=30, pickup="10:00"), lt(minutes=10, pickup="10:32")]
    _, warnings = plan_times(legs, NOW)
    assert len(warnings) == 1 and warnings[0].startswith("Tight connection")
    _, warnings = plan_times([lt(minutes=30, pickup="10:00"), lt("kid", 10, pickup="10:05")], NOW)
    assert warnings == []


def test_unspecified_times_chain_or_leave_now():
    timed, _ = plan_times([lt(minutes=30, pickup="09:00"), lt(minutes=20)], NOW)
    assert hm(timed[1].pickup) == "09:35"  # previous arrival 9:30 + 5 min gap
    timed, _ = plan_times([lt(minutes=30)], NOW)
    assert hm(timed[0].pickup) == "08:05"
    timed, _ = plan_times([lt(minutes=30, pickup="09:00"), lt(minutes=20, asap=True)], NOW)
    assert [(t.index, hm(t.pickup)) for t in timed] == [(1, "08:05"), (0, "09:00")]


def test_all_times_passed_moves_the_plan_to_tomorrow():
    late = NOW.replace(hour=19)
    timed, warnings = plan_times([lt(pickup="15:15"), lt("kid", arrive="18:00")], late)
    assert warnings[0] == "3:15 PM has already passed today, so the plan is for tomorrow."
    assert all(t.pickup.date() == dt.date(2026, 11, 19) for t in timed)


def test_partially_passed_times_warn():
    mid = NOW.replace(hour=16)
    timed, warnings = plan_times([lt("kid", 20, pickup="15:15"), lt(minutes=55, arrive="18:00")], mid)
    kid = next(t for t in timed if t.leg.passenger == "kid")
    assert kid.warnings == ["Pickup time 3:15 PM has already passed; dispatching as soon as possible."]
    assert hm(kid.pickup) == "16:05" and warnings == []


def test_arrive_by_too_soon():
    soon = NOW.replace(hour=17, minute=30)
    (t,), _ = plan_times([lt(minutes=55, arrive="18:00")], soon)
    assert hm(t.pickup) == "17:35"
    assert t.warnings == ["Can't arrive by 6:00 PM: 55 min trip, earliest arrival about 6:30 PM."]
    (t,), _ = plan_times([lt(minutes=20, arrive="18:00")], soon)
    assert t.warnings == ["Leave now to make 6:00 PM (20 min trip)."]


def test_pickup_and_arrive_by_together():
    (t,), _ = plan_times([lt(minutes=60, pickup="17:30", arrive="18:00")], NOW)
    assert t.warnings == ["Leaving at 5:30 PM arrives about 6:30 PM, after the 6:00 PM arrive-by."]


def test_tomorrow_offset_and_formatting():
    (t,), _ = plan_times([lt(pickup="05:30", day=1)], NOW)
    assert t.pickup.date() == dt.date(2026, 11, 19)
    assert fmt_time(dt.datetime(2026, 1, 1, 0, 5, tzinfo=IST)) == "12:05 AM"
    assert fmt_time(dt.datetime(2026, 1, 1, 12, 0, tzinfo=IST)) == "12:00 PM"
    assert short("Office - Manyata Tech Park") == "Office" and short("") == "?"
    assert t.name == "Office -> Airport"


@pytest.mark.parametrize("now_utc_hour", [0, 10, 20])
def test_now_in_any_timezone_is_read_as_ist(now_utc_hour):
    now = dt.datetime(2026, 11, 18, now_utc_hour, 0, tzinfo=dt.timezone.utc)
    timed, _ = plan_times([lt(minutes=10)], now)
    assert timed[0].pickup.utcoffset() == dt.timedelta(hours=5, minutes=30)
