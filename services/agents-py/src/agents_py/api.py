"""Agent platform endpoints (proxied by dispatch-go under /v1/agents*)."""
from __future__ import annotations

from typing import Optional

from fastapi import APIRouter, Query

from src.agents_py.core import registry
from src.agents_py.core.events import get_event_store
from src.agents_py.core.llm import get_llm

router = APIRouter()


@router.get("/v1/agents")
async def list_agents():
    store = await get_event_store()
    agents = await registry.list_agents(store)
    return {
        "agents": agents,
        "active": sum(a["status"] == "active" for a in agents),
        "total": len(agents),
        "events_backend": store.backend,
    }


@router.get("/v1/agents/events")
async def agent_events(limit: int = Query(50, ge=1, le=500), agent: Optional[str] = None):
    store = await get_event_store()
    events = await store.recent(limit=limit, agent=agent or None)
    return {"events": events, "count": len(events), "backend": store.backend}


@router.get("/v1/agents/llm/budget")
async def llm_budget():
    return await (await get_llm()).budget()
