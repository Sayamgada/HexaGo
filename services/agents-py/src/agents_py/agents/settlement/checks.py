"""Deterministic anomaly checks over dispatch-go ledger entries and payouts.

No LLM here: every check is a pure function of the entries, the payouts and
the clock. ``compute_split`` mirrors dispatch-go's ``ledger.ComputeSplit``
exactly (integer paise, floors, remainder to the platform), so a posted
split that disagrees with it is a real mismatch.
"""
from __future__ import annotations

import datetime as dt
import re
from collections import defaultdict
from dataclasses import asdict, dataclass, field
from typing import Any, Dict, Iterable, List, Optional

PAYOUT_DELAY_LIMIT_S = 5.0
HELD_LIMIT_S = 600.0
UNUSUAL_TIP_PAISE = 50_000          # ₹500
UNUSUAL_TIP_SHARE = 0.5             # or a tip above half the fare
REFUND_LOOP_COUNT = 3               # refunds on one ride

SPLIT_MISMATCH = "split_mismatch"
UNBALANCED = "unbalanced_entry"
REFUND_LOOP = "refund_loop"
UNUSUAL_TIP = "unusual_tip"
PAYOUT_DELAYED = "payout_delayed"
HELD_TOO_LONG = "held_payout_stale"

SEVERITY = {UNBALANCED: 1.0, SPLIT_MISMATCH: 0.9, REFUND_LOOP: 0.7, HELD_TOO_LONG: 0.6, PAYOUT_DELAYED: 0.5,
            UNUSUAL_TIP: 0.4}

DEFAULT_RATES = {"driver_share_bps": 7500, "fleet_operator_share_bps": 8000, "city_levy_bps": 200, "gst_bps": 500}


def rupees(paise: int) -> str:
    sign = "-" if paise < 0 else ""
    paise = abs(int(paise))
    return f"{sign}₹{paise // 100}.{paise % 100:02d}"


_FRACTION = re.compile(r"(\.\d{1,6})\d*")


def parse_ts(v: Any) -> Optional[dt.datetime]:
    """RFC 3339 from Go (nanoseconds, 'Z') -> aware datetime; None if missing or invalid."""
    if not v or not isinstance(v, str):
        return None
    s = _FRACTION.sub(r"\1", v.replace("Z", "+00:00"), count=1)
    try:
        t = dt.datetime.fromisoformat(s)
    except ValueError:
        return None
    return t if t.tzinfo else t.replace(tzinfo=dt.timezone.utc)


def compute_split(fare: int, promo: int = 0, tip: int = 0, fleet: bool = False,
                  rates: Optional[Dict[str, int]] = None) -> Dict[str, int]:
    """dispatch-go ledger.ComputeSplit, in paise."""
    r = {**DEFAULT_RATES, **(rates or {})}
    share_bps = r["fleet_operator_share_bps"] if fleet else r["driver_share_bps"]
    promo = min(promo, fare)
    payee = fare * share_bps // 10_000
    levy = fare * r["city_levy_bps"] // 10_000
    rest = fare - payee - levy
    gst = rest * r["gst_bps"] // (10_000 + r["gst_bps"])
    return {"fare_paise": fare, "promo_paise": promo, "tip_paise": tip, "rider_charge_paise": fare - promo + tip,
            "payee_share_paise": payee, "city_levy_paise": levy, "gst_paise": gst, "platform_fee_paise": rest - gst}


@dataclass
class Finding:
    check: str
    ride_id: str
    detail: str
    driver_id: str = ""
    entry_id: str = ""
    payout_id: str = ""
    severity: float = 0.5
    facts: Dict[str, Any] = field(default_factory=dict)

    @property
    def key(self) -> str:
        """Stable identity: the same anomaly is reported (and explained) once."""
        return f"{self.check}:{self.payout_id or self.entry_id or self.ride_id}"

    def template(self) -> str:
        return TEMPLATES[self.check].format(ride=self.ride_id[:8] or "?", detail=self.detail)

    def to_dict(self) -> Dict[str, Any]:
        return {**asdict(self), "key": self.key}


TEMPLATES = {
    SPLIT_MISMATCH: "Ride {ride}: the posted split does not match the settlement formula ({detail}). "
                    "Check for a rate change or a manual posting.",
    UNBALANCED: "Ride {ride}: a journal entry does not sum to zero ({detail}). The ledger must be audited.",
    REFUND_LOOP: "Ride {ride}: repeated refunds ({detail}). Possible refund abuse or a retrying client.",
    UNUSUAL_TIP: "Ride {ride}: unusually large tip ({detail}). Verify with the rider before the next payout.",
    PAYOUT_DELAYED: "Ride {ride}: the instant payout was slow ({detail}). Check the payout worker and gateway.",
    HELD_TOO_LONG: "Ride {ride}: a payout has been held for review for too long ({detail}). "
                   "The safety incident should be resolved so the driver is paid.",
}


def _legs(entry: Dict[str, Any]) -> Dict[str, int]:
    out: Dict[str, int] = defaultdict(int)
    for p in entry.get("postings") or []:
        out[p.get("memo", "")] += int(p.get("amount_paise", 0))
    return out


def check_entry(entry: Dict[str, Any]) -> List[Finding]:
    """Balance check for every entry; split and tip checks for ride settlements."""
    found: List[Finding] = []
    ride, eid = entry.get("ride_id") or "", entry.get("id") or ""
    meta = entry.get("meta") or {}
    driver = meta.get("driver_id") or ""
    total = sum(int(p.get("amount_paise", 0)) for p in entry.get("postings") or [])
    if total != 0:
        found.append(Finding(UNBALANCED, ride, f"postings sum to {rupees(total)}", driver, eid,
                             severity=SEVERITY[UNBALANCED], facts={"sum_paise": total}))
    split = meta.get("split")
    if entry.get("kind") != "ride_settlement" or not split:
        return found
    fare, promo, tip = int(split.get("fare_paise", 0)), int(split.get("promo_paise", 0)), int(split.get("tip_paise", 0))
    want = compute_split(fare, promo, tip, fleet=split.get("payee_type") == "fleet_operator_payable",
                         rates=meta.get("rates") or None)
    legs = _legs(entry)
    actual = {
        "payee_share_paise": -legs["payee_share"], "tip_paise": -legs["tip"], "city_levy_paise": -legs["city_levy"],
        "gst_paise": -legs["gst"], "platform_fee_paise": -legs["platform_fee"], "promo_paise": legs["promo"],
        "rider_charge_paise": legs["rider_wallet"] + legs["card"],
    }
    diffs = {k: (actual[k], want[k]) for k in actual if actual[k] != want[k]}
    diffs.update({f"recorded_{k}": (int(split.get(k, 0)), want[k]) for k in want
                  if k in split and int(split.get(k, 0)) != want[k]})
    if diffs:
        detail = ", ".join(f"{k} {rupees(a)} vs expected {rupees(b)}" for k, (a, b) in sorted(diffs.items())[:3])
        found.append(Finding(SPLIT_MISMATCH, ride, detail, driver, eid, severity=SEVERITY[SPLIT_MISMATCH],
                             facts={"differences": {k: {"posted": a, "expected": b} for k, (a, b) in diffs.items()}}))
    if tip >= UNUSUAL_TIP_PAISE or (fare > 0 and tip > fare * UNUSUAL_TIP_SHARE):
        found.append(Finding(UNUSUAL_TIP, ride, f"tip {rupees(tip)} on a {rupees(fare)} fare", driver, eid,
                             severity=SEVERITY[UNUSUAL_TIP], facts={"tip_paise": tip, "fare_paise": fare}))
    return found


def check_refunds(entries: Iterable[Dict[str, Any]]) -> List[Finding]:
    by_ride: Dict[str, List[Dict[str, Any]]] = defaultdict(list)
    for e in entries:
        if e.get("kind") == "refund" and e.get("ride_id"):
            by_ride[e["ride_id"]].append(e)
    found = []
    for ride, refunds in by_ride.items():
        if len(refunds) < REFUND_LOOP_COUNT:
            continue
        amount = sum(-int(p.get("amount_paise", 0)) for e in refunds for p in e.get("postings") or []
                     if p.get("memo") == "refund")
        driver = ((refunds[0].get("meta") or {}).get("driver_id")) or ""
        found.append(Finding(REFUND_LOOP, ride, f"{len(refunds)} refunds totalling {rupees(amount)}", driver,
                             severity=SEVERITY[REFUND_LOOP], facts={"refunds": len(refunds), "amount_paise": amount}))
    return found


def check_payout(p: Dict[str, Any], now: dt.datetime) -> List[Finding]:
    created = parse_ts(p.get("created_at"))
    if created is None:
        return []
    ride, pid, driver, status = p.get("ride_id") or "", p.get("id") or "", p.get("driver_id") or "", p.get("status")
    if status == "held_for_review":
        held = parse_ts(p.get("held_at")) or created
        age = (now - held).total_seconds()
        if age > HELD_LIMIT_S:
            return [Finding(HELD_TOO_LONG, ride, f"{rupees(int(p.get('amount_paise', 0)))} held {age / 60:.0f} min "
                                                 f"(incident {p.get('incident_id') or '?'})", driver, payout_id=pid,
                            severity=SEVERITY[HELD_TOO_LONG],
                            facts={"held_s": round(age), "incident_id": p.get("incident_id")})]
        return []
    if p.get("released_at"):
        return []  # released payouts were waiting on purpose
    sent = parse_ts(p.get("sent_at"))
    delay = ((sent or now) - created).total_seconds()
    if delay > PAYOUT_DELAY_LIMIT_S:
        state = "sent after" if sent else "still pending after"
        return [Finding(PAYOUT_DELAYED, ride, f"{state} {delay:.1f} s", driver, payout_id=pid,
                        severity=SEVERITY[PAYOUT_DELAYED], facts={"delay_s": round(delay, 3), "status": status})]
    return []


def run_checks(entries: List[Dict[str, Any]], payouts: List[Dict[str, Any]], now: dt.datetime) -> List[Finding]:
    """All checks; findings sorted by severity (highest first), deduplicated by key."""
    found: Dict[str, Finding] = {}
    for f in [*(x for e in entries for x in check_entry(e)), *check_refunds(entries),
              *(x for p in payouts for x in check_payout(p, now))]:
        found.setdefault(f.key, f)
    return sorted(found.values(), key=lambda f: (-f.severity, f.key))
