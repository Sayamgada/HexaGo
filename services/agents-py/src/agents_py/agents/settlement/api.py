"""Settlement agent endpoints (reached through the gateway's /v1/agents/* proxy).

    GET  /v1/agents/settlement/findings   last run + flagged items (template or LLM explanation)
    POST /v1/agents/settlement/scan       run the audit now
"""
from __future__ import annotations

from typing import Any, Dict

from fastapi import APIRouter

from src.agents_py.agents.settlement.agent import AGENT, get_agent
from src.agents_py.core.graph import agent

router = APIRouter()


@agent(AGENT, "Audits the double-entry ledger every 60 s: split vs formula, balance, refund loops, unusual tips, "
              "payout delays and stale held payouts. Checks are deterministic; the LLM only explains flagged items.")
async def scan() -> Dict[str, Any]:
    return await get_agent().scan()


@router.get("/v1/agents/settlement/findings")
async def findings():
    return get_agent().view()


@router.post("/v1/agents/settlement/scan")
async def run_scan():
    run = await scan()
    return {**get_agent().view(), "last_run": run}
