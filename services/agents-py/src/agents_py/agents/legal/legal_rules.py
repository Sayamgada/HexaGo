"""Legal & Jurisdiction: jurisdiction rules, the deterministic regulation extractor, validation and diff.

Pure functions only (no I/O). A jurisdiction's rules:

    max_surge_multiplier    float, 1.0 .. 5.0   (read live by pricing through ``get_surge_cap``)
    min_insurance_coverage  int INR per passenger
    max_driving_hours       int hours per 24 h, 1 .. 24
    av_allowed_zones        H3 cell ids where autonomous vehicles may operate
    required_documents      canonical document keys (``driving_licence``, ``vehicle_rc``, ...)
    city_levy_pct           float percent of the fare, 0 .. 20

A change set (``RuleChanges``) holds a new value or ``None`` per rule; the two
list rules carry a mode: ``set`` replaces the list, ``add`` / ``remove`` edit it.
The LLM fills a change set from pasted regulation text; ``extract_rules`` is the
regex fallback, and its explicit numbers win over the LLM's (``reconcile``).
Every value is bounds-checked (``validate_changes``) before ``diff_rules``
turns it into old -> new rows.
"""
from __future__ import annotations

import copy
import re
from typing import Any, Dict, List, Optional, Tuple

import h3
from pydantic import BaseModel, Field

SCALAR_FIELDS = ("max_surge_multiplier", "min_insurance_coverage", "max_driving_hours", "city_levy_pct")
LIST_FIELDS = ("av_allowed_zones", "required_documents")
RULE_FIELDS = ("max_surge_multiplier", "min_insurance_coverage", "max_driving_hours", "av_allowed_zones",
               "required_documents", "city_levy_pct")
MODE_OF = {"av_allowed_zones": "av_zones_mode", "required_documents": "documents_mode"}
MODES = ("set", "add", "remove")

BOUNDS: Dict[str, Tuple[float, float]] = {
    "max_surge_multiplier": (1.0, 5.0),
    "min_insurance_coverage": (0, 10_000_000_000),
    "max_driving_hours": (1, 24),
    "city_levy_pct": (0.0, 20.0),
}

CITY_ALIASES: Dict[str, Tuple[str, ...]] = {
    "bengaluru": ("bengaluru", "bangalore", "blr", "karnataka"),
    "mumbai": ("mumbai", "bombay", "maharashtra"),
    "delhi": ("new delhi", "delhi", "ncr"),
}

# Canonical document keys and the phrases that name them, in display order.
DOCUMENTS: Tuple[Tuple[str, str], ...] = (
    ("driving_licence", r"driving\s+licen[cs]e|\bdl\b|\blicen[cs]e"),
    ("vehicle_rc", r"registration|\brc\b"),
    ("insurance_certificate", r"insurance\s+(?:certificate|policy|document)"),
    ("puc_certificate", r"\bpuc\b|pollution"),
    ("police_verification", r"police\s+(?:verification|clearance)"),
    ("fitness_certificate", r"fitness\s+certificate"),
    ("commercial_permit", r"\bpermit\b"),
    ("driver_badge", r"\bbadge\b"),
    ("aadhaar", r"aadhaa?r"),
)


def _cells(*points: Tuple[float, float]) -> List[str]:
    return [h3.latlng_to_cell(lat, lng, 7) for lat, lng in points]


_DOCS = ["driving_licence", "vehicle_rc", "insurance_certificate", "puc_certificate", "police_verification",
         "commercial_permit"]
_SEED: Dict[str, Dict[str, Any]] = {
    "bengaluru": {
        "city": "bengaluru", "name": "Bengaluru", "state": "Karnataka", "authority": "Karnataka Transport Department",
        "rules": {"max_surge_multiplier": 2.5, "min_insurance_coverage": 500_000, "max_driving_hours": 12,
                  # Electronic City, Whitefield, Bellandur (H3 res 7)
                  "av_allowed_zones": _cells((12.8452, 77.6602), (12.9698, 77.7500), (12.9260, 77.6762)),
                  "required_documents": list(_DOCS), "city_levy_pct": 2.0},
    },
    "mumbai": {
        "city": "mumbai", "name": "Mumbai", "state": "Maharashtra", "authority": "Maharashtra Motor Vehicles Department",
        "rules": {"max_surge_multiplier": 2.0, "min_insurance_coverage": 500_000, "max_driving_hours": 12,
                  "av_allowed_zones": _cells((19.0676, 72.8677)),  # Bandra Kurla Complex
                  "required_documents": list(_DOCS), "city_levy_pct": 2.0},
    },
    "delhi": {
        "city": "delhi", "name": "Delhi", "state": "Delhi", "authority": "Delhi Transport Department",
        "rules": {"max_surge_multiplier": 2.0, "min_insurance_coverage": 500_000, "max_driving_hours": 12,
                  "av_allowed_zones": [],
                  "required_documents": [*_DOCS, "fitness_certificate"], "city_levy_pct": 2.0},
    },
}


def seed_jurisdictions() -> Dict[str, Dict[str, Any]]:
    """A fresh copy of the three seeded cities (Bengaluru, Mumbai, Delhi)."""
    return copy.deepcopy(_SEED)


class RuleChanges(BaseModel):
    """The structured rule changes in a regulation text (the LLM's JSON schema). null = not mentioned."""

    max_surge_multiplier: Optional[float] = Field(None, description="surge cap, e.g. 1.5 for 1.5x the base fare")
    min_insurance_coverage: Optional[float] = Field(None, description="minimum insurance cover in INR")
    max_driving_hours: Optional[float] = Field(None, description="maximum driving hours per driver per 24 h")
    av_allowed_zones: Optional[List[str]] = Field(None, description="H3 cell ids for autonomous vehicles")
    av_zones_mode: str = Field("set", description="set | add | remove")
    required_documents: Optional[List[str]] = Field(None, description="snake_case document keys")
    documents_mode: str = Field("set", description="set | add | remove")
    city_levy_pct: Optional[float] = Field(None, description="city levy, percent of the fare")
    summary: str = Field("", max_length=500)


def empty_changes() -> Dict[str, Any]:
    return RuleChanges().model_dump()


def has_changes(changes: Dict[str, Any]) -> bool:
    return any(changes.get(f) is not None for f in RULE_FIELDS)


# ---------------------------------------------------------------- cities

def normalize_city(name: Optional[str]) -> Optional[str]:
    """'Bangalore' -> 'bengaluru'; None for an unknown city."""
    key = (name or "").strip().lower()
    for city, aliases in CITY_ALIASES.items():
        if key == city or key in aliases:
            return city
    return None


def infer_city(text: str) -> Optional[str]:
    """The first seeded city (or its state) named in the text."""
    low = (text or "").lower()
    hits = []
    for city, aliases in CITY_ALIASES.items():
        for a in aliases:
            m = re.search(rf"\b{re.escape(a)}\b", low)
            if m:
                hits.append((m.start(), city))
    return min(hits)[1] if hits else None


# ---------------------------------------------------------------- fallback extractor

_SENTENCE = re.compile(r"(?<=[.!?;])\s+|\n+")
_MULT = re.compile(r"(\d+(?:\.\d+)?)\s*(?:x|×|times)(?![a-z])")
_CAP_WORDS = re.compile(r"cap|ceiling|limit|max|exceed|not more than|no more than|up to|restrict")
_HOURS = re.compile(r"(\d{1,2})\s*(?:driving\s+|working\s+|duty\s+)?hours?\b")
_DRIVING = re.compile(r"\bdriv|\bwork|\bduty|\bshift")
_RUPEES = re.compile(r"(?:\brs\.?|\binr|₹)\s*([\d,]+(?:\.\d+)?)\s*(lakh|lac|crore|cr)?\b")
_INDIAN_UNITS = re.compile(r"(\d+(?:\.\d+)?)\s*(lakh|lac|crore)\b")
_PERCENT = re.compile(r"(\d+(?:\.\d+)?)\s*(?:%|per\s*cent|percent)")
_CELL = re.compile(r"\b[0-9a-f]{15}\b")
_AV = re.compile(r"autonomous|driverless|self-driving|robotaxi|\bavs?\b")
_DOC_CONTEXT = re.compile(r"document|carry|require|must hold|must have|mandatory|valid")
_REMOVE = re.compile(r"no longer|not required|removed|dropped|exempt|waived")
_ADD = re.compile(r"additionally|\balso\b|in addition|now require|must now|new requirement")
_BAN = re.compile(r"prohibit|\bban(?:ned|s)?\b|not permitted|not allowed|suspend|withdrawn|barred")
_CITYWIDE = re.compile(r"citywide|city-wide|entire|all zones|everywhere|until further")
_ONLY = re.compile(r"\bonly\b|exclusively|restricted to")

_UNIT = {"lakh": 100_000, "lac": 100_000, "crore": 10_000_000, "cr": 10_000_000}


def _sentences(text: str) -> List[str]:
    return [s.strip().lower() for s in _SENTENCE.split(text or "") if s.strip()]


def _rupees(s: str) -> Optional[int]:
    m = _RUPEES.search(s)
    if m:
        value = float(m.group(1).replace(",", ""))
        return int(round(value * _UNIT.get(m.group(2) or "", 1)))
    m = _INDIAN_UNITS.search(s)
    if m:
        return int(round(float(m.group(1)) * _UNIT[m.group(2)]))
    return None


def _documents(s: str) -> List[str]:
    found = []
    for key, pattern in DOCUMENTS:
        m = re.search(pattern, s)
        if m:
            found.append((m.start(), key))
    return [k for _, k in sorted(found)]


def _list_mode(s: str) -> str:
    if _REMOVE.search(s):
        return "remove"
    if _ADD.search(s):
        return "add"
    return "set" if re.search(r"required documents|documents required|following documents|:|\bonly\b", s) else "add"


def extract_rules(text: str) -> Dict[str, Any]:
    """Deterministic rule extraction from regulation text (the LLM fallback).

    Reads one sentence at a time; the first sentence that states a rule wins.
    Handles "surge cap of 1.8x", "capped at 1.5x the base fare", "shall not
    exceed 2 times", "maximum 10 driving hours", "Rs 15,00,000" / "10 lakh" /
    "INR 750000" insurance, "levy ... 3.5%", document lists and AV zones (H3 cells).
    """
    out = empty_changes()
    for s in _sentences(text):
        if out["max_surge_multiplier"] is None and "surge" in s and _CAP_WORDS.search(s):
            m = _MULT.search(s)
            if m:
                out["max_surge_multiplier"] = float(m.group(1))
        if out["max_driving_hours"] is None and _DRIVING.search(s) and _CAP_WORDS.search(s):
            m = _HOURS.search(s)
            if m:
                out["max_driving_hours"] = int(m.group(1))
        if out["min_insurance_coverage"] is None and re.search(r"insur|\bcover", s):
            amount = _rupees(s)
            if amount is not None:
                out["min_insurance_coverage"] = amount
        if out["city_levy_pct"] is None and re.search(r"\blevy|\bcess", s):
            m = _PERCENT.search(s)
            if m:
                out["city_levy_pct"] = float(m.group(1))
        if out["av_allowed_zones"] is None and _AV.search(s):
            cells = [c for c in _CELL.findall(s) if h3.is_valid_cell(c)]
            banned = bool(_BAN.search(s))
            if cells:
                out["av_allowed_zones"] = list(dict.fromkeys(cells))
                out["av_zones_mode"] = "remove" if banned else ("set" if _ONLY.search(s) else "add")
            elif banned and _CITYWIDE.search(s):
                out["av_allowed_zones"], out["av_zones_mode"] = [], "set"
        if out["required_documents"] is None and _DOC_CONTEXT.search(s):
            docs = _documents(s)
            if docs:
                out["required_documents"], out["documents_mode"] = docs, _list_mode(s)
    return out


# ---------------------------------------------------------------- validation, reconcile, diff

def _doc_key(name: str) -> str:
    low = str(name).strip().lower()
    for key, pattern in DOCUMENTS:
        if low == key or re.search(pattern, low):
            return key
    return re.sub(r"[^a-z0-9]+", "_", low).strip("_")


def validate_changes(changes: Dict[str, Any]) -> Tuple[Dict[str, Any], List[Dict[str, Any]]]:
    """Coerce and bounds-check a change set. Returns (valid changes, rejected [{field, value, reason}])."""
    valid = empty_changes()
    rejected: List[Dict[str, Any]] = []
    for f in SCALAR_FIELDS:
        v = changes.get(f)
        if v is None:
            continue
        try:
            num = float(v)
        except (TypeError, ValueError):
            rejected.append({"field": f, "value": v, "reason": "not a number"})
            continue
        lo, hi = BOUNDS[f]
        if not lo <= num <= hi:
            rejected.append({"field": f, "value": v, "reason": f"outside {lo:g}..{hi:g}"})
            continue
        valid[f] = int(round(num)) if f in ("min_insurance_coverage", "max_driving_hours") else round(num, 2)
    for f in LIST_FIELDS:
        mode_key = MODE_OF[f]
        mode = str(changes.get(mode_key) or "set").lower()
        valid[mode_key] = mode if mode in MODES else "set"
        v = changes.get(f)
        if v is None:
            continue
        items = [str(x).strip() for x in (v if isinstance(v, (list, tuple)) else [v]) if str(x).strip()]
        if f == "av_allowed_zones":
            good = [c.lower() for c in items if h3.is_valid_cell(c.lower())]
            bad = [c for c in items if not h3.is_valid_cell(c.lower())]
            if bad:
                rejected.append({"field": f, "value": bad, "reason": "not H3 cell ids"})
            if bad and not good:
                continue
            valid[f] = list(dict.fromkeys(good))
        else:
            valid[f] = list(dict.fromkeys(k for k in (_doc_key(x) for x in items) if k))
    return valid, rejected


def reconcile(llm: Dict[str, Any], rules: Dict[str, Any]) -> Tuple[Dict[str, Any], List[str]]:
    """Merge the LLM's change set with the extractor's: an explicit number in the text wins over the LLM.

    Returns (merged, fields where the LLM disagreed with the text and was overridden)."""
    merged = {**empty_changes(), **llm}
    overridden = []
    for f in SCALAR_FIELDS:
        r = rules.get(f)
        if r is None:
            continue
        if merged.get(f) is None:
            merged[f] = r
            continue
        try:
            same = abs(float(merged[f]) - float(r)) < 1e-9
        except (TypeError, ValueError):
            same = False
        if not same:
            merged[f] = r
            overridden.append(f)
    for f in LIST_FIELDS:
        if merged.get(f) is None and rules.get(f) is not None:
            merged[f], merged[MODE_OF[f]] = rules[f], rules.get(MODE_OF[f], "set")
    return merged, overridden


def _target_list(old: List[str], new: List[str], mode: str) -> List[str]:
    if mode == "add":
        return [*old, *[x for x in new if x not in old]]
    if mode == "remove":
        return [x for x in old if x not in new]
    return list(new)


def diff_rules(old: Dict[str, Any], changes: Dict[str, Any]) -> List[Dict[str, Any]]:
    """Old -> new rows for every rule the (validated) change set actually changes, in RULE_FIELDS order."""
    diff = []
    for f in RULE_FIELDS:
        new = changes.get(f)
        if new is None:
            continue
        cur = old.get(f)
        if f in LIST_FIELDS:
            cur = list(cur or [])
            target = _target_list(cur, list(new), str(changes.get(MODE_OF[f]) or "set"))
            if set(target) != set(cur):
                diff.append({"field": f, "old": cur, "new": target})
        elif cur != new:
            diff.append({"field": f, "old": cur, "new": new})
    return diff


def apply_diff(rules: Dict[str, Any], diff: List[Dict[str, Any]]) -> Dict[str, Any]:
    out = copy.deepcopy(rules)
    for d in diff:
        out[d["field"]] = copy.deepcopy(d["new"])
    return out


def describe_diff(diff: List[Dict[str, Any]]) -> str:
    """'max_surge_multiplier 2.5 -> 1.5; required_documents +b'."""
    if not diff:
        return "no rule changes"
    parts = []
    for d in diff:
        if isinstance(d["new"], list):
            old, new = list(d["old"] or []), d["new"]
            delta = [f"+{x}" for x in new if x not in old] + [f"-{x}" for x in old if x not in new]
            parts.append(f"{d['field']} {' '.join(delta)}")
        else:
            parts.append(f"{d['field']} {d['old']} -> {d['new']}")
    return "; ".join(parts)
