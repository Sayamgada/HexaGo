"""Deterministic booking parser (regex + keyword rules).

Runs whenever the LLM is disabled, out of budget, rate-limited or times out,
and must fully handle the common patterns:

* "from X to Y", "to Y", "reach Y", "take me to Y"
* "pick up <who> from X [at T]" then "drop them at Y"
* "at 3:15 PM", "at 15:15", "at 3 pm", "at noon", "now", "in 20 minutes", "tomorrow"
* "arrive by 6", "to arrive by 6:00 PM", "reach by 10 pm", "by 6pm"
* tiers: "high-tier sedan" / "premium" -> premium, "sedan", "auto", "mini", "XL" / "SUV",
  "self-driving" -> av, "pool" / "shared"
* passengers: "for me", "for my kid", "my daughter", "my mom", pronouns them / him / her

A bare hour without am/pm is read as a daytime hour: 1-7 -> PM, 8-11 -> AM.
Output is a `ParsedRequest` with local 24h HH:MM times.
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from typing import List, Optional, Tuple

from src.agents_py.agents.booking.models import LegSpec, ParsedRequest

# ---- times ----
_T = r"(?:noon|midday|midnight|\d{1,2}(?::\d{2}|\.\d{2})?\s*(?:a\.?m\.?|p\.?m\.?)?(?![\d:\w]))"
ARRIVE_RE = re.compile(
    rf"(?:\b(?:so as\s+)?to\s+)?\b(?:arrive|arriving|reach|reaching|get there|be there|land|be at \w+)\s+"
    rf"(?:by|before|at|latest by)\s+(?P<t>{_T})|\b(?:by|before|no later than)\s+(?P<t2>{_T})", re.I)
PICKUP_RE = re.compile(rf"(?:\b(?:at|@|around)\s+)(?P<t>{_T})|(?P<t2>\b\d{{1,2}}(?::\d{{2}})\s*(?:[ap]\.?m\.?)?)"
                       rf"|(?P<t3>\b\d{{1,2}}\s*(?:a\.?m\.?|p\.?m\.?))(?!\w)", re.I)
ASAP_RE = re.compile(r"\b(?:right now|now|asap|immediately|as soon as possible)\b", re.I)
IN_RE = re.compile(r"\bin\s+(?P<n>\d{1,3}|an?|half an)\s*(?P<u>min(?:ute)?s?|hours?|hrs?)\b", re.I)
TOMORROW_RE = re.compile(r"\b(?:tomorrow|tmrw|tmr)\b", re.I)
TODAY_RE = re.compile(r"\b(?:today|tonight|this (?:morning|afternoon|evening))\b", re.I)


def parse_clock(raw: str) -> Optional[str]:
    """'3:15 PM' -> '15:15'; '6' -> '18:00' (daytime rule); 'noon' -> '12:00'."""
    s = re.sub(r"(\d)\.(\d)", r"\1:\2", raw.strip().lower())
    if s in ("noon", "midday"):
        return "12:00"
    if s == "midnight":
        return "00:00"
    m = re.match(r"^(\d{1,2})(?::(\d{2}))?\s*([ap])?\.?\s*m?\.?$", s.replace(" ", ""))
    if not m:
        return None
    h, mi, ap = int(m.group(1)), int(m.group(2) or 0), m.group(3)
    if mi > 59 or h > 23:
        return None
    if ap:
        if h == 0 or h > 12:
            return None
        h = h % 12 + (12 if ap == "p" else 0)
    elif m.group(2) is None or h <= 12:
        if 1 <= h <= 7:
            h += 12
    return f"{h:02d}:{mi:02d}"


# ---- tiers ----
TIER_PATTERNS: List[Tuple[str, str]] = [
    ("premium", r"high[- ]?(?:tier|end)|top[- ]?tier|premium|luxury|lux|executive|business[- ]class"),
    ("xl", r"xl|suv|six[- ]seater|6[- ]seater|seven[- ]seater|7[- ]seater|innova|van"),
    ("av", r"self[- ]driving|autonomous|driverless|robo[- ]?taxi|robotaxi|av"),
    ("pool", r"pool(?:ed)?|shared(?: ride)?|share(?:d)? cab|carpool"),
    ("auto", r"auto[- ]?rickshaw|auto|rickshaw|tuk[- ]?tuk"),
    ("sedan", r"sedan|comfort"),
    ("mini", r"mini|hatchback|cheapest|economy|budget"),
]
_VEHICLE = r"(?:\s+(?:car|cab|taxi|ride|vehicle|one))?"
_TIER_RE = [(t, re.compile(rf"(?:\b(?:in|with|using|via|by)\s+)?(?:\b(?:a|an|the)\s+)?\b(?:{p})\b(?:\s+(?:sedan|car))?"
                           rf"{_VEHICLE}", re.I))
            for t, p in TIER_PATTERNS]
GENERIC_VEHICLE = re.compile(r"(?:\b(?:in|with)\s+)?\b(?:a|an)\s+(?:cab|car|taxi|ride)\b|\b(?:cab|taxi)\b", re.I)


def find_tier(text: str) -> Tuple[Optional[str], str]:
    for tier, rx in _TIER_RE:
        m = rx.search(text)
        if m:
            return tier, (text[:m.start()] + " " + text[m.end():])
    return None, text


# ---- passengers ----
KID_WORDS = r"kid|kids|son|daughter|child|children|boy|girl|little one"
RELATIONS = (r"wife|husband|mom|mother|mum|dad|father|parents|friend|colleague|boss|grandma|grandmother|"
             r"grandpa|grandfather|guest|sister|brother|partner")
WHO_RE = re.compile(rf"\b(?:for\s+)?(?:my|our)\s+(?P<who>{KID_WORDS}|{RELATIONS})\b", re.I)
FOR_ME_RE = re.compile(r"\bfor\s+(?:me|myself)\b", re.I)
ME_RE = re.compile(r"\b(?:me|i|myself|i'm|i am)\b", re.I)
PRONOUN_RE = re.compile(r"\b(?:them|him|her)\b", re.I)


def _passenger(word: str) -> str:
    w = word.lower()
    if re.fullmatch(KID_WORDS, w):
        return "kid"
    return {"mum": "mom", "mother": "mom", "father": "dad"}.get(w, w)


# ---- places ----
PICKUP_FROM_RE = re.compile(r"\bpick\s*(?:-\s*)?(?:up)?\s+(?P<who>.*?)\s*(?:up\s+)?\bfrom\s+(?P<a>.+)$", re.I)
DROP_RE = re.compile(r"\bdrop\s+(?:(?P<who>.+?)\s+)?(?:off\s+)?(?:at|to|near|in)\s+(?P<b>.+)$", re.I)
FROM_TO_RE = re.compile(r"\bfrom\s+(?P<a>.+?)\s+(?:to|till|until|towards)\s+(?P<b>.+)$", re.I)
TO_RE = re.compile(r"\b(?:to|towards|reach|for)\s+(?P<b>.+)$", re.I)
FROM_RE = re.compile(r"\bfrom\s+(?P<a>.+)$", re.I)
_LEAD_VERBS = re.compile(r"^(?:(?:reach|get to|go to|head to|be at|the|my|our|a|an)\s+)+", re.I)
_TRAIL = re.compile(r"\s+(?:please|pls|thanks|thank you|asap|now)\s*$", re.I)

CLAUSE_SPLIT = re.compile(
    r"\s*(?:,|;|\.)?\s*\b(?:and\s+)?then\b\s*|\s*;\s*|\.\s+(?=[A-Za-z])"
    r"|\s*,?\s*\band\s+(?=(?:drop|pick|book|get|send|take|call|order|also)\b)"
    r"|\s*,\s*(?=(?:drop|pick|book|get|send|take|call|order|also)\b)", re.I)


def split_clauses(text: str) -> List[str]:
    return [c.strip(" ,.") for c in CLAUSE_SPLIT.split(text) if c and c.strip(" ,.")]


def clean_place(raw: Optional[str]) -> Optional[str]:
    if raw is None:
        return None
    s = re.sub(r"\s+", " ", raw).strip(" ,.!?")
    s = _TRAIL.sub("", s)
    s = _LEAD_VERBS.sub("", s).strip(" ,.")
    s = re.sub(r"\b(?:for|with|in|at|by)\s*$", "", s, flags=re.I).strip(" ,.")
    return s or None


@dataclass
class _Clause:
    text: str
    pickup_at: Optional[str] = None
    arrive_by: Optional[str] = None
    asap: bool = False
    minutes_from_now: Optional[int] = None
    tier: Optional[str] = None
    tomorrow: bool = False


def _strip_times(c: _Clause) -> None:
    t = c.text
    if TOMORROW_RE.search(t):
        c.tomorrow = True
        t = TOMORROW_RE.sub(" ", t)
    t = TODAY_RE.sub(" ", t)
    m = ARRIVE_RE.search(t)
    if m:
        c.arrive_by = parse_clock(m.group("t") or m.group("t2"))
        t = t[:m.start()] + " " + t[m.end():]
    m = IN_RE.search(t)
    if m:
        n = m.group("n").lower()
        qty = 1 if n in ("a", "an") else (30 if n == "half an" else int(n))
        mins = qty * 60 if m.group("u").lower().startswith("h") and n != "half an" else qty
        c.minutes_from_now = mins
        t = t[:m.start()] + " " + t[m.end():]
    m = PICKUP_RE.search(t)
    if m:
        c.pickup_at = parse_clock(m.group("t") or m.group("t2") or m.group("t3"))
        t = t[:m.start()] + " " + t[m.end():]
    if ASAP_RE.search(t):
        c.asap = True
        t = ASAP_RE.sub(" ", t)
    c.text = re.sub(r"\s+", " ", t).strip(" ,.")


def parse_answer_time(text: str, now_hhmm_minutes: int) -> Tuple[Optional[str], bool]:
    """A clarifying answer like "3:30 PM", "now" or "in 20 minutes" -> (HH:MM or None, asap).

    ``now_hhmm_minutes`` is the local time of day in minutes, for relative answers.
    """
    t = text or ""
    m = IN_RE.search(t)
    if m:
        n = m.group("n").lower()
        qty = 1 if n in ("a", "an") else (30 if n == "half an" else int(n))
        mins = qty * 60 if m.group("u").lower().startswith("h") and n != "half an" else qty
        at = (now_hhmm_minutes + mins) % (24 * 60)
        return f"{at // 60:02d}:{at % 60:02d}", False
    if ASAP_RE.search(t):
        return None, True
    m = ARRIVE_RE.search(t) or PICKUP_RE.search(t)
    if m:
        raw = next(g for g in m.groups() if g)
        return parse_clock(raw), False
    bare = re.fullmatch(rf"\s*({_T})\s*", t, re.I)
    return (parse_clock(bare.group(1)), False) if bare else (None, False)


def parse(text: str) -> ParsedRequest:
    """Rule-based parse of a ride request into legs."""
    legs: List[LegSpec] = []
    ambiguities: List[str] = []
    last_passenger = "me"
    last_label: Optional[str] = None
    tomorrow_all = bool(TOMORROW_RE.search(text or ""))
    global_tier: Optional[str] = None

    for raw in split_clauses(text or ""):
        c = _Clause(raw)
        _strip_times(c)
        c.tier, rest = find_tier(c.text)
        rest = GENERIC_VEHICLE.sub(" ", rest)

        # passenger
        passenger, label = None, None
        m = WHO_RE.search(rest)
        if m:
            passenger, label = _passenger(m.group("who")), m.group(0).replace("for ", "").strip()
        elif FOR_ME_RE.search(rest):
            passenger = "me"
        elif PRONOUN_RE.search(rest):
            passenger, label = last_passenger, last_label
        rest = FOR_ME_RE.sub(" ", rest)
        rest = re.sub(rf"\bfor\s+(?:my|our)\s+(?:{KID_WORDS}|{RELATIONS})\b", " ", rest, flags=re.I)
        rest = re.sub(r"\s+", " ", rest).strip()

        origin = dest = None
        drop_only = False
        m = PICKUP_FROM_RE.search(rest)
        if m:
            who = m.group("who")
            wm = WHO_RE.search(who or "")
            if wm:
                passenger, label = _passenger(wm.group("who")), wm.group(0).strip()
            elif PRONOUN_RE.search(who or ""):
                passenger, label = passenger or last_passenger, label or last_label
            elif ME_RE.search(who or ""):
                passenger = "me"
            src = m.group("a")
            ft = re.search(r"\s+(?:and\s+)?(?:to|drop(?:\s+\w+)?\s+(?:at|to))\s+(?P<b>.+)$", src, re.I)
            if ft:
                origin, dest = clean_place(src[:ft.start()]), clean_place(ft.group("b"))
            else:
                origin = clean_place(src)
        elif (m := DROP_RE.search(rest)):
            who = m.group("who") or ""
            wm = WHO_RE.search(who)
            if wm:
                passenger, label = _passenger(wm.group("who")), wm.group(0).strip()
            elif ME_RE.search(who) and not PRONOUN_RE.search(who):
                passenger = "me"
            else:
                passenger, label = passenger or last_passenger, label or last_label
            dest = clean_place(m.group("b"))
            drop_only = True
        elif (m := FROM_TO_RE.search(rest)):
            origin, dest = clean_place(m.group("a")), clean_place(m.group("b"))
        elif (m := TO_RE.search(rest)):
            dest = clean_place(m.group("b"))
        elif (m := FROM_RE.search(rest)):
            origin = clean_place(m.group("a"))

        passenger = passenger or ("me" if ME_RE.search(rest) or not legs else last_passenger)
        if c.tier and not (origin or dest or c.pickup_at or c.arrive_by) and legs:
            global_tier = global_tier or c.tier  # "make it a sedan"
            legs[-1].tier = legs[-1].tier or c.tier
            continue
        if not (origin or dest or c.pickup_at or c.arrive_by or c.asap or c.minutes_from_now):
            if c.tier:
                global_tier = c.tier
            continue

        # "drop them at Y" completes the open leg of the same passenger.
        if drop_only and legs and legs[-1].destination is None and legs[-1].passenger == passenger:
            leg = legs[-1]
            leg.destination = dest
            leg.arrive_by = leg.arrive_by or c.arrive_by
            leg.tier = leg.tier or c.tier
            continue
        if drop_only and legs:
            prev = next((lg for lg in reversed(legs) if lg.passenger == passenger), legs[-1])
            origin = prev.destination

        legs.append(LegSpec(
            passenger=passenger, passenger_label=label, origin=origin, destination=dest,
            pickup_at=c.pickup_at, arrive_by=c.arrive_by, day_offset=1 if (c.tomorrow or tomorrow_all) else 0,
            tier=c.tier, asap=c.asap or c.minutes_from_now is not None,
        ))
        last_passenger, last_label = passenger, label

    if not legs:
        ambiguities.append("no destination found")
    for i, leg in enumerate(legs):
        if leg.destination is None:
            ambiguities.append(f"leg {i + 1}: no destination")
    return ParsedRequest(legs=legs, tier=global_tier, ambiguities=ambiguities)
