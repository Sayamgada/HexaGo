"""Legal & Jurisdiction agent: regulation text in, rule changes applied, audited and live.

``ingest(text, city)``:

1. The LLM extracts a structured change set (``RuleChanges``) through
   ``core.llm.complete_json`` (guardrails, budget, cache). The deterministic
   regex extractor (``legal_rules.extract_rules``) is its fallback and also
   cross-checks it: an explicit number in the text wins over a disagreeing LLM.
2. Values are bounds-checked; out-of-range ones are rejected, never applied.
3. The diff (old -> new) against the city's current rules is applied at once,
   written to the ``legal_audit`` log and to ``agent_events`` (Ops feed).

Pricing reads the surge cap through ``get_surge_cap(city)``: cached 30 s per
process and invalidated here on every local change, so the next surge-map
recompute (every 30 s) and every fare quote use the new cap.
"""
from __future__ import annotations

import time
from typing import Any, Callable, Dict, Optional, Tuple

from src.agents_py.agents.legal.legal_rules import (
    RULE_FIELDS, RuleChanges, apply_diff, describe_diff, diff_rules, extract_rules, has_changes, infer_city,
    normalize_city, reconcile, validate_changes,
)
from src.agents_py.agents.legal.legal_store import LegalStore
from src.agents_py.config import settings
from src.agents_py.core.events import EventStore, get_event_store
from src.agents_py.core.graph import agent
from src.agents_py.core.llm import LLMClient, get_llm

AGENT = "legal_jurisdiction"
CAP_TTL_S = 30.0
DEFAULT_SURGE_CAP = 2.5
SAMPLE_NOTICE = ("Karnataka Transport Dept notice: surge pricing for app-based cabs is capped at 1.5x the base fare "
                 "with immediate effect.")


class UnknownCity(ValueError):
    pass


def _prompt(text: str, city_name: str, rules: Dict[str, Any]) -> str:
    return (
        "You are HexaGo's Legal & Jurisdiction agent. Extract the ride-hailing rule changes stated in the "
        f"regulation text below for {city_name}. Only report rules the text explicitly sets; use null for every "
        "rule it does not mention. Units: max_surge_multiplier as a multiple of the base fare (1.5 for 1.5x); "
        "min_insurance_coverage in INR (1 lakh = 100000, 1 crore = 10000000); max_driving_hours per driver per "
        "24 h; city_levy_pct in percent of the fare; av_allowed_zones only as H3 cell ids that appear in the text; "
        "required_documents as snake_case keys (driving_licence, vehicle_rc, insurance_certificate, "
        "puc_certificate, police_verification, fitness_certificate, commercial_permit, driver_badge). For the two "
        "lists, mode is set (replace), add or remove. summary: one sentence on what changes.\n"
        f"Current rules: {rules}\n"
        f"Regulation text:\n\"\"\"\n{text[:4000]}\n\"\"\""
    )


class LegalAgent:
    def __init__(self, store: Optional[LegalStore] = None, llm: Optional[LLMClient] = None,
                 events: Optional[EventStore] = None):
        self.store = store or LegalStore()
        self._llm, self._events = llm, events

    async def llm(self) -> LLMClient:
        if self._llm is None:
            self._llm = await get_llm()
        return self._llm

    async def events(self) -> EventStore:
        if self._events is None:
            self._events = await get_event_store()
        return self._events

    def resolve_city(self, text: str, city: Optional[str]) -> str:
        if city and city.strip():
            key = normalize_city(city)
            if key is None:
                raise UnknownCity(f"unknown city {city!r}: one of bengaluru, mumbai, delhi")
            return key
        return infer_city(text) or normalize_city(settings.CITY_NAME) or "bengaluru"

    async def ingest(self, text: str, city: Optional[str] = None, actor: str = "api") -> Dict[str, Any]:
        start = time.perf_counter()
        key = self.resolve_city(text, city)
        current = await self.store.get(key)
        if current is None:
            raise UnknownCity(f"unknown city {key!r}")
        extracted = extract_rules(text)
        res = await (await self.llm()).complete_json(
            _prompt(text, current["name"], current["rules"]), RuleChanges, AGENT, lambda: extracted)
        changes, overridden = dict(res.data), []
        if not res.used_fallback:
            changes, overridden = reconcile(changes, extracted)
        valid, rejected = validate_changes(changes)
        diff = diff_rules(current["rules"], valid)
        updated = current
        if diff:
            updated = await self.store.update_rules(key, apply_diff(current["rules"], diff), by=AGENT)
            invalidate_caps(key)
        source = "rules" if res.used_fallback else "llm"
        model = res.model if (res.model and not res.used_fallback) else "rule_extractor"
        summary = str(changes.get("summary") or "") or (
            f"Rule extractor found: {describe_diff(diff)}." if diff else "No rule changes found in the text.")
        audit = await self.store.add_audit({
            "city": key, "text": text[:4000], "diff": diff, "rejected": rejected, "overridden": overridden,
            "applied": bool(diff), "source": source, "model": model, "fallback_reason": res.fallback_reason,
            "summary": summary, "version": updated["version"], "actor": actor,
            "detected": [f for f in RULE_FIELDS if valid.get(f) is not None] if has_changes(valid) else [],
        })
        decision = (f"{current['name']}: {describe_diff(diff)} applied" if diff
                    else f"{current['name']}: no rule changes in the text")
        notes = []
        if overridden:
            notes.append(f"text overrode the LLM on {', '.join(overridden)}")
        if rejected:
            notes.append("rejected " + ", ".join(f"{r['field']}={r['value']} ({r['reason']})" for r in rejected))
        event = await (await self.events()).record_event(
            AGENT, f"regulation text for {current['name']} ({len(text)} chars)", decision,
            summary + (" " + "; ".join(notes) + "." if notes else ""),
            0.9 if source == "llm" else 0.8, (time.perf_counter() - start) * 1000, res.used_fallback,
            res.fallback_reason,
            {"kind": "legal_rule_change" if diff else "legal_no_change", "city": key, "diff": diff,
             "audit_id": audit["id"], "model": model, "tokens": res.tokens, "version": updated["version"]},
        )
        return {
            "city": key, "jurisdiction": updated, "diff": diff, "rejected": rejected, "overridden": overridden,
            "applied": bool(diff), "audit_id": audit["id"], "source": source, "model": model,
            "used_fallback": res.used_fallback, "fallback_reason": res.fallback_reason, "summary": summary,
            "event_id": event.get("id"),
        }


_agent: Optional[LegalAgent] = None


async def get_agent() -> LegalAgent:
    global _agent
    from src.agents_py.core import infra

    if _agent is None:
        _agent = LegalAgent(LegalStore(pool=await infra.get_pool()))
    elif _agent.store.pool is None:
        _agent.store.pool = await infra.get_pool()
    return _agent


def set_agent(a: Optional[LegalAgent]) -> None:
    global _agent
    _agent = a


@agent(AGENT, "Turns pasted regulation text into rule changes per jurisdiction (LLM, regex fallback), applies the "
              "diff with an audit row; pricing reads the surge cap live.")
async def ingest(text: str, city: Optional[str] = None) -> Dict[str, Any]:
    return await (await get_agent()).ingest(text, city)


# ---------------------------------------------------------------- surge cap hook (read by pricing)

_caps: Dict[str, Tuple[float, float]] = {}  # city -> (cap, fetched at, monotonic s)


def invalidate_caps(city: Optional[str] = None) -> None:
    if city is None:
        _caps.clear()
    else:
        _caps.pop(city, None)


async def get_surge_cap(city: Optional[str] = None, default: float = DEFAULT_SURGE_CAP,
                        clock: Callable[[], float] = time.monotonic) -> float:
    """The jurisdiction's max_surge_multiplier, cached CAP_TTL_S (30 s).

    ``default`` when LEGAL_ENABLED is off or the city is unknown; the last
    known value (else ``default``) when the rules store fails.
    """
    if not settings.LEGAL_ENABLED:
        return default
    key = normalize_city(city or settings.CITY_NAME)
    if key is None:
        return default
    now = clock()
    hit = _caps.get(key)
    if hit and now - hit[1] < CAP_TTL_S:
        return hit[0]
    try:
        j = await (await get_agent()).store.get(key)
    except Exception:  # noqa: BLE001 - pricing must never fail on the rules store
        return hit[0] if hit else default
    cap = float(j["rules"]["max_surge_multiplier"]) if j else default
    _caps[key] = (cap, now)
    return cap
