"""Demand Forecast: the pure model (binning, Holt smoothing, seasonality, scenario boosts, earnings, templates)."""
import datetime as dt

import h3
import pytest

from src.agents_py.agents.forecast import model as fm

T0 = dt.datetime(2026, 11, 19, 12, 30, tzinfo=dt.timezone.utc)  # 18:00 IST, evening ramp
CENTER = (12.9716, 77.5946)
MG_ROAD = (12.9756, 77.6050)
OUTER = (13.0300, 77.6400)  # ~8 km from the centre


def reqs(p, minutes_ago, n=1):
    """n requests at p, at each of the given minutes ago."""
    return [fm.RideRequest(p[0], p[1], T0 - dt.timedelta(minutes=m, seconds=i))
            for m in minutes_ago for i in range(n)]


def cell(p):
    return h3.latlng_to_cell(p[0], p[1], fm.RESOLUTION)


def by_cell(cells):
    return {c["h3"]: c for c in cells}


# ---- binning and smoothing ----

def test_bin_requests_counts_oldest_first_and_skips_out_of_window():
    rs = reqs(MG_ROAD, [1, 2, 7, 119]) + reqs(MG_ROAD, [121, -1]) + [fm.RideRequest(float("nan"), 1, T0)]
    bins = fm.bin_requests(rs, T0)
    series = bins[cell(MG_ROAD)]
    assert len(series) == fm.WINDOW_S // fm.BIN_S == 24
    assert series[-1] == 2 and series[-2] == 1 and series[0] == 1
    assert sum(series) == 4


def test_cell_of_rejects_bad_points():
    assert fm.cell_of(MG_ROAD[0], MG_ROAD[1]) == cell(MG_ROAD)
    assert fm.cell_of(None, 1) is None
    assert fm.cell_of(200, 1) is None


def test_holt_tracks_level_and_trend():
    assert fm.holt([]) == (0.0, 0.0)
    level, trend = fm.holt([2.0] * 24)
    assert level == pytest.approx(2.0) and trend == pytest.approx(0.0)
    level, trend = fm.holt([float(i) for i in range(24)])
    assert trend > 0.5 and level > 15


def test_smooth_forecast_rises_with_trend_and_never_goes_negative():
    rising = [float(i) / 4 for i in range(24)]
    assert fm.smooth_forecast(rising, 6) > fm.smooth_forecast(rising, 0) > 0
    falling = [6.0] * 20 + [0.0] * 4
    assert fm.smooth_forecast(falling, 6) >= 0.0
    assert fm.smooth_forecast([], 3) == 0.0


# ---- seasonality ----

def test_profile_is_piecewise_and_wraps_midnight():
    assert fm.hourly_profile(19.0) == pytest.approx(1.5)
    assert fm.hourly_profile(24.5) == pytest.approx(fm.hourly_profile(0.5))
    assert fm.hourly_profile(4.0) < fm.hourly_profile(8.0)


def test_seasonality_follows_the_evening_ramp():
    # 18:00 IST -> 18:30 IST: the curve rises into the 19:00 peak.
    assert fm.seasonality(T0, 30) > fm.seasonality(T0, 15) > 1.0
    assert fm.seasonality(T0, 0) == pytest.approx(1.0)
    late = dt.datetime(2026, 11, 19, 16, 30, tzinfo=dt.timezone.utc)  # 22:00 IST, winding down
    assert fm.seasonality(late, 30) < 1.0


def test_ist_hour():
    assert fm.ist_hour(T0) == pytest.approx(18.0)


# ---- history ----

def test_history_rates_average_over_days_present():
    hist = [fm.RideRequest(MG_ROAD[0], MG_ROAD[1], T0 - dt.timedelta(days=d, minutes=m))
            for d in (1, 2) for m in (5, 10, 20)]
    rates, days = fm.history_rates(hist, T0)
    assert days == 2
    # 3 requests per day inside a one-hour window -> 3 req/h.
    assert rates[cell(MG_ROAD)] == pytest.approx(3.0)
    assert fm.history_rates([], T0) == ({}, 0)
    stale = [fm.RideRequest(MG_ROAD[0], MG_ROAD[1], T0 - dt.timedelta(days=1, hours=3))]
    assert fm.history_rates(stale, T0) == ({}, 0)


# ---- scenarios ----

def test_rain_boost_is_everywhere_and_stronger_further_out():
    sc = [fm.Scenario("rain_storm", T0 + dt.timedelta(minutes=5))]
    near = fm.boost_for(sc, CENTER[0], CENTER[1], 0, T0)
    far = fm.boost_for(sc, OUTER[0], OUTER[1], 0, T0)
    assert 1.3 < near < far <= 2.0


def test_scenario_boost_fades_after_it_ends():
    sc = [fm.Scenario("rain_storm", T0 + dt.timedelta(minutes=5))]
    now = fm.boost_for(sc, OUTER[0], OUTER[1], 0, T0)
    later = fm.boost_for(sc, OUTER[0], OUTER[1], 30, T0)
    assert 1.0 < later < now


def test_concert_boost_is_local_to_the_venue():
    sc = [fm.Scenario("concert_exit", T0 + dt.timedelta(minutes=5), lat=fm.VENUE[0], lng=fm.VENUE[1])]
    at_venue = fm.boost_for(sc, fm.VENUE[0], fm.VENUE[1], 0, T0)
    away = fm.boost_for(sc, OUTER[0], OUTER[1], 0, T0)
    assert at_venue > 3.0 and away == pytest.approx(1.0)
    # Without a spike location the default venue, shifted to the city centre, is used.
    shifted = fm.Scenario("concert_exit", T0 + dt.timedelta(minutes=5))
    assert fm.boost_for([shifted], fm.VENUE[0], fm.VENUE[1], 0, T0, center=fm.DEFAULT_CENTER) > 3.0


def test_rush_hour_and_unknown_scenarios():
    rush = [fm.Scenario("rush_hour", T0 + dt.timedelta(minutes=5))]
    assert fm.boost_for(rush, CENTER[0], CENTER[1], 0, T0) == pytest.approx(fm.RUSH_BOOST)
    assert fm.boost_for([fm.Scenario("safety_incident", T0)], CENTER[0], CENTER[1], 0, T0) == 1.0
    assert fm.boost_for([], CENTER[0], CENTER[1], 0, T0) == 1.0


def test_parse_scenarios_from_sim_config():
    cfg = {"scenarios": [{"name": "concert_exit", "active_until": "2026-11-19T12:35:00Z"},
                         {"name": "rain_storm", "active_until": "bad"}, {"nope": 1}],
           "spikes": [{"name": "concert exit", "lat": 13.0, "lng": 77.59}]}
    sc = fm.parse_scenarios(cfg)
    assert [s.name for s in sc] == ["concert_exit"]
    assert sc[0].lat == 13.0 and sc[0].until == T0 + dt.timedelta(minutes=5)
    assert fm.parse_scenarios({}) == [] and fm.parse_scenarios(None) == []


# ---- earnings and score ----

def test_demand_score_is_bounded_and_monotonic():
    assert fm.demand_score(0) == 0.0
    assert fm.demand_score(fm.REF_RATE) == pytest.approx(0.5)
    assert fm.demand_score(5) < fm.demand_score(50) < 1.0


def test_expected_earnings_saturate_and_share_with_supply():
    assert fm.expected_earnings(0, 0) == 0
    lonely = fm.expected_earnings(6, 0)
    crowded = fm.expected_earnings(6, 5)
    assert lonely > crowded > 0
    assert fm.expected_earnings(1000, 0) == round(fm.AVG_FARE_INR * fm.DRIVER_SHARE * fm.MAX_TRIPS_PER_HR)


def test_supply_by_cell_counts_idle_cars_only():
    snap = {"drivers": [{"lat": MG_ROAD[0], "lng": MG_ROAD[1], "status": "idle"},
                        {"lat": MG_ROAD[0], "lng": MG_ROAD[1], "status": "on_trip"},
                        {"lat": "x", "lng": 1, "status": "idle"}]}
    assert fm.supply_by_cell(snap) == {cell(MG_ROAD): 1}
    assert fm.supply_by_cell(None) == {}


# ---- the forecast ----

def test_forecast_shapes_cells_for_every_horizon():
    recent = reqs(MG_ROAD, range(0, 120, 2)) + reqs(OUTER, [3, 40])
    out = fm.forecast(recent, [], T0, supply={cell(MG_ROAD): 2})
    assert set(out) == set(fm.HORIZONS)
    cells = out[15]
    top = cells[0]
    assert top["h3"] == cell(MG_ROAD)
    assert len(top["boundary"]) == 6 and all(len(p) == 2 for p in top["boundary"])
    assert 0 < top["demand_score"] < 1
    assert top["expected_requests_per_hr"] > 10
    assert top["expected_earnings_per_hr"] > 0
    assert top["supply"] == 2 and top["recent_requests"] == 15
    assert top["trend"] in ("rising", "steady", "falling")
    assert top["explanation"] and top["explanation_source"] == "template"
    assert [c["demand_score"] for c in cells] == sorted((c["demand_score"] for c in cells), reverse=True)
    assert fm.forecast(recent, [], T0) == fm.forecast(recent, [], T0)  # pure


def test_forecast_rain_storm_raises_every_cell():
    recent = reqs(MG_ROAD, range(0, 120, 3)) + reqs(OUTER, range(0, 120, 10))
    calm = by_cell(fm.forecast(recent, [], T0)[0])
    rain = by_cell(fm.forecast(recent, [], T0, scenarios=[fm.Scenario("rain_storm", T0 + dt.timedelta(minutes=5))])[0])
    for h in calm:
        assert rain[h]["expected_requests_per_hr"] > calm[h]["expected_requests_per_hr"]
    assert "rain" in rain[cell(OUTER)]["explanation"]


def test_forecast_blends_history_and_includes_history_only_cells():
    hist = [fm.RideRequest(OUTER[0], OUTER[1], T0 - dt.timedelta(days=1, minutes=m)) for m in range(0, 30)]
    out = fm.forecast(reqs(MG_ROAD, [1]), hist, T0)
    c = by_cell(out[0])[cell(OUTER)]
    assert c["expected_requests_per_hr"] > 0 and c["recent_requests"] == 0
    assert "same hour" in c["explanation"]


def test_forecast_rising_trend_and_cap():
    rising = [fm.RideRequest(MG_ROAD[0], MG_ROAD[1], T0 - dt.timedelta(minutes=m, seconds=s))
              for m in range(0, 30) for s in range(0, 60, 60 // max(1, 6 - m // 5))]
    c = fm.forecast(rising, [], T0)[15][0]
    assert c["trend"] == "rising"
    many = [fm.RideRequest(12.9 + i * 0.01, 77.5 + j * 0.01, T0 - dt.timedelta(minutes=1))
            for i in range(20) for j in range(20)]
    assert len(fm.forecast(many, [], T0, max_cells=50)[0]) == 50


def test_hotspots_rank_by_earnings():
    recent = reqs(MG_ROAD, range(0, 120, 2)) + reqs(OUTER, range(0, 120, 4))
    cells = fm.forecast(recent, [], T0, supply={cell(MG_ROAD): 30})[15]
    hs = fm.hotspots(cells, 2)
    assert hs[0]["h3"] == cell(OUTER)  # fewer cars competing
    assert len(hs) == 2


def test_template_explanation_mentions_horizon_and_falling_trend():
    c = {"h3": "x", "expected_requests_per_hr": 4.0, "recent_requests": 9, "trend": "falling",
         "expected_earnings_per_hr": 120, "history_rate": 0.0}
    text = fm.template_explanation(c, 30, [])
    assert "+30 min" in text and "easing" in text and "₹120" in text
    assert "now" in fm.template_explanation({**c, "trend": "steady"}, 0, [])
    concert = [fm.Scenario("concert_exit", T0), fm.Scenario("rush_hour", T0)]
    text = fm.template_explanation({**c, "trend": "rising"}, 15, concert)
    assert "concert" in text and "rush hour" in text and "building" in text
