"""Deterministic Settlement checks: the Go split formula, mismatches, loops, tips, payout timing."""
import datetime as dt

import pytest

from src.agents_py.agents.settlement import checks as c

NOW = dt.datetime(2026, 11, 18, 10, 0, tzinfo=dt.timezone.utc)


def ts(seconds_ago: float) -> str:
    """Go-style RFC 3339 with nanoseconds."""
    return (NOW - dt.timedelta(seconds=seconds_ago)).strftime("%Y-%m-%dT%H:%M:%S.%f") + "123Z"


def ride_entry(ride="ride-1", fare=25000, promo=0, tip=0, fleet=False, tamper=None, wallet=0):
    s = c.compute_split(fare, promo, tip, fleet=fleet)
    payee_type = "fleet_operator_payable" if fleet else "driver_payable"
    split = {**s, "payee_type": payee_type, "payee_share_bps": 8000 if fleet else 7500}
    legs = {"payee_share": -s["payee_share_paise"], "tip": -tip, "city_levy": -s["city_levy_paise"],
            "gst": -s["gst_paise"], "platform_fee": -s["platform_fee_paise"], "promo": s["promo_paise"],
            "rider_wallet": wallet, "card": s["rider_charge_paise"] - wallet}
    for k, v in (tamper or {}).items():
        legs[k] += v
    postings = [{"memo": k, "amount_paise": v, "account": f"x:{k}"} for k, v in legs.items() if v]
    return {"id": f"je_{ride}", "kind": "ride_settlement", "ride_id": ride, "created_at": ts(30),
            "meta": {"driver_id": "d_001", "split": split, "rates": dict(c.DEFAULT_RATES)}, "postings": postings}


@pytest.mark.parametrize("fare,promo,tip,fleet,want", [
    (25000, 0, 0, False, (18750, 500, 273, 5477, 25000)),
    (12345, 0, 0, False, (9258, 246, 135, 2706, 12345)),
    (1, 0, 0, False, (0, 0, 0, 1, 1)),
    (25000, 0, 0, True, (20000, 500, 214, 4286, 25000)),
    (33333, 0, 0, True, (26666, 666, 285, 5716, 33333)),
    (4000, 9000, 0, False, (3000, 80, 43, 877, 0)),          # promo capped at the fare
    (18001, 2500, 1000, True, (14400, 360, 154, 3087, 16501)),
])
def test_compute_split_matches_go_table(fare, promo, tip, fleet, want):
    s = c.compute_split(fare, promo, tip, fleet=fleet)
    assert (s["payee_share_paise"], s["city_levy_paise"], s["gst_paise"], s["platform_fee_paise"],
            s["rider_charge_paise"]) == want
    assert s["payee_share_paise"] + tip + s["city_levy_paise"] + s["gst_paise"] + s["platform_fee_paise"] == fare + tip
    assert s["rider_charge_paise"] + s["promo_paise"] == fare + tip


def test_clean_entries_raise_nothing():
    entries = [ride_entry("a"), ride_entry("b", 33333, fleet=True), ride_entry("c", 18001, 2500, 1000, wallet=5000),
               {"id": "je_t", "kind": "topup", "postings": [{"memo": "topup", "amount_paise": 5},
                                                             {"memo": "topup", "amount_paise": -5}]}]
    assert c.run_checks(entries, [], NOW) == []


def test_split_mismatch_and_unbalanced():
    bad = ride_entry("bad", tamper={"platform_fee": -100, "card": 100})  # balanced, but the split is wrong
    found = c.run_checks([bad], [], NOW)
    assert [f.check for f in found] == [c.SPLIT_MISMATCH]
    assert "platform_fee_paise" in found[0].detail and found[0].entry_id == "je_bad"
    assert found[0].key == "split_mismatch:je_bad" and "Ride bad" in found[0].template()

    broken = ride_entry("brk", tamper={"gst": 7})
    checks = [f.check for f in c.run_checks([broken], [], NOW)]
    assert checks == [c.UNBALANCED, c.SPLIT_MISMATCH]  # most severe first

    recorded = ride_entry("rec")
    recorded["meta"]["split"]["gst_paise"] += 1  # the stored split disagrees with the formula
    f = c.run_checks([recorded], [], NOW)[0]
    assert f.check == c.SPLIT_MISMATCH and "recorded_gst_paise" in f.facts["differences"]


def test_unusual_tips():
    assert [f.check for f in c.run_checks([ride_entry("big", tip=50_000)], [], NOW)] == [c.UNUSUAL_TIP]
    assert [f.check for f in c.run_checks([ride_entry("ratio", fare=4000, tip=2500)], [], NOW)] == [c.UNUSUAL_TIP]
    assert c.run_checks([ride_entry("ok", tip=5000)], [], NOW) == []


def test_refund_loops():
    def refund(i, ride="loop"):
        return {"id": f"je_r{i}", "kind": "refund", "ride_id": ride, "meta": {"driver_id": "d_9"},
                "postings": [{"memo": "refund", "amount_paise": -1000}, {"memo": "platform_fee", "amount_paise": 1000}]}
    assert c.run_checks([refund(1), refund(2)], [], NOW) == []
    found = c.run_checks([refund(1), refund(2), refund(3), refund(4, "other")], [], NOW)
    assert len(found) == 1 and found[0].check == c.REFUND_LOOP and found[0].driver_id == "d_9"
    assert found[0].facts == {"refunds": 3, "amount_paise": 3000} and "₹30.00" in found[0].detail


def test_payout_timing():
    sent_fast = {"id": "po_a", "ride_id": "a", "status": "sent", "created_at": ts(60), "sent_at": ts(59.7)}
    sent_slow = {"id": "po_b", "ride_id": "b", "status": "sent", "created_at": ts(60), "sent_at": ts(50)}
    pending_old = {"id": "po_c", "ride_id": "c", "status": "pending", "created_at": ts(9)}
    pending_new = {"id": "po_d", "ride_id": "d", "status": "pending", "created_at": ts(1)}
    held_new = {"id": "po_e", "ride_id": "e", "status": "held_for_review", "created_at": ts(900), "held_at": ts(300)}
    held_old = {"id": "po_f", "ride_id": "f", "status": "held_for_review", "created_at": ts(900), "held_at": ts(700),
                "incident_id": "inc_1", "amount_paise": 18750}
    released = {"id": "po_g", "ride_id": "g", "status": "sent", "created_at": ts(900), "sent_at": ts(10),
                "released_at": ts(10)}
    no_ts = {"id": "po_h", "status": "pending"}
    found = c.run_checks([], [sent_fast, sent_slow, pending_old, pending_new, held_new, held_old, released, no_ts], NOW)
    by_key = {f.key: f for f in found}
    assert set(by_key) == {"payout_delayed:po_b", "payout_delayed:po_c", "held_payout_stale:po_f"}
    assert "sent after 10.0 s" in by_key["payout_delayed:po_b"].detail
    assert "still pending" in by_key["payout_delayed:po_c"].detail
    assert "₹187.50 held 12 min (incident inc_1)" in by_key["held_payout_stale:po_f"].detail
    assert found[0].check == c.HELD_TOO_LONG  # sorted by severity


def test_helpers():
    assert c.rupees(123456) == "₹1234.56" and c.rupees(-5) == "-₹0.05"
    assert c.parse_ts(None) is None and c.parse_ts(123) is None and c.parse_ts("garbage") is None
    assert c.parse_ts("2026-11-18T10:00:00") == NOW  # naive -> UTC
    assert c.parse_ts("2026-11-18T10:00:00.123456789Z").microsecond == 123456
    d = c.Finding(c.UNUSUAL_TIP, "r", "x").to_dict()
    assert d["key"] == "unusual_tip:r" and d["check"] == c.UNUSUAL_TIP
