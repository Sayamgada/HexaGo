"""Small helpers for building HexaGo agents on LangGraph.

    @agent("surge_equilibrium", "Prices hex cells from supply/demand")
    async def surge(event): ...

    node = run_with_fallback(llm_node, fallback_node)   # never raises
    graph = build_graph(node)                            # START -> decide -> END
    state = await graph.ainvoke({"agent": "surge_equilibrium", "input": {...}})

`run_with_fallback` returns a node that tries the LLM path and, if it raises
(``LLMUnavailable`` carries a guardrail reason; anything else is "error"),
runs the deterministic fallback and marks the state ``used_fallback=True``.
An LLM node that already fell back inside `complete_json` reports that itself
by returning ``used_fallback`` / ``fallback_reason``.
"""
from __future__ import annotations

import inspect
import logging
import time
from typing import Any, Awaitable, Callable, Dict, Optional, TypedDict, Union

from langgraph.graph import END, START, StateGraph

from src.agents_py.core import registry

log = logging.getLogger("hexago.graph")


class AgentState(TypedDict, total=False):
    agent: str
    input: Dict[str, Any]
    output: Dict[str, Any]
    decision: str
    reasoning: str
    confidence: float
    used_fallback: bool
    fallback_reason: Optional[str]
    latency_ms: float
    metadata: Dict[str, Any]


Node = Callable[[AgentState], Union[Dict[str, Any], Awaitable[Dict[str, Any]]]]


class LLMUnavailable(Exception):
    """Raised by an LLM node to request the fallback with a specific reason."""

    def __init__(self, reason: str = "error"):
        super().__init__(reason)
        self.reason = reason


async def _call(node: Node, state: AgentState) -> Dict[str, Any]:
    out = node(state)
    if inspect.isawaitable(out):
        out = await out
    return dict(out or {})


def run_with_fallback(llm_node: Node, fallback_node: Node) -> Callable[[AgentState], Awaitable[Dict[str, Any]]]:
    async def node(state: AgentState) -> Dict[str, Any]:
        start = time.perf_counter()
        try:
            update = await _call(llm_node, state)
            update.setdefault("used_fallback", False)
            update.setdefault("fallback_reason", None)
        except Exception as err:  # noqa: BLE001 - every failure must end in the fallback
            reason = err.reason if isinstance(err, LLMUnavailable) else "error"
            if not isinstance(err, LLMUnavailable):
                log.warning("agent %s llm node failed: %s", state.get("agent", "?"), err)
            update = await _call(fallback_node, state)
            update["used_fallback"] = True
            update["fallback_reason"] = reason
        update.setdefault("latency_ms", round((time.perf_counter() - start) * 1000, 3))
        return update

    return node


def build_graph(node: Callable[[AgentState], Any], name: str = "decide"):
    """Compile a single-step graph START -> node -> END over AgentState."""
    g = StateGraph(AgentState)
    g.add_node(name, node)
    g.add_edge(START, name)
    g.add_edge(name, END)
    return g.compile()


def agent(name: str, description: str):
    """Register the decorated callable as the implementation of agent `name`."""

    def wrap(fn):
        reg = registry.register(name, description, fn)
        fn.agent_id = reg.id
        fn.agent_name = reg.name
        return fn

    return wrap
