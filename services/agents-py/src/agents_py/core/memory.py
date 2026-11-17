"""Agent episodic memory: remember(agent, text) / recall(agent, query, k).

Backends, best first:

1. Postgres + pgvector: rows in ``agent_memory`` with an ``embedding vector(768)``
   column, recalled by cosine distance.
2. Postgres without the extension: the same table without embeddings, recalled
   by keyword overlap over the agent's most recent rows.
3. No database (or it fails): a bounded in-memory list per agent, recalled by
   cosine similarity when both sides have embeddings, else by keywords.

Embeddings go through `LLMClient.embed`, so they obey the same cost guardrails;
when one is blocked the memory is stored without a vector and recall uses keywords.
"""
from __future__ import annotations

import collections
import json
import logging
import math
import re
from typing import Any, Deque, Dict, List, Optional, Sequence

from src.agents_py.core import infra

log = logging.getLogger("hexago.memory")

EMBEDDING_DIM = 768  # gemini/text-embedding-004
KEYWORD_SCAN_ROWS = 500

_WORD = re.compile(r"[a-z0-9]+")


def _words(text: str) -> set:
    return {w for w in _WORD.findall((text or "").lower()) if len(w) > 1}


def _stem(word: str) -> str:
    for suffix in ("ing", "ed", "es", "s"):
        if len(word) > len(suffix) + 2 and word.endswith(suffix):
            return word[: -len(suffix)]
    return word


def keyword_score(query: str, text: str) -> float:
    """Share of query words (crudely stemmed) that appear in text, 0..1."""
    q = {_stem(w) for w in _words(query)}
    if not q:
        return 0.0
    t = {_stem(w) for w in _words(text)}
    return round(len(q & t) / len(q), 4)


def cosine(a: Sequence[float], b: Sequence[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    return round(dot / (na * nb), 6) if na and nb else 0.0


def _vec_literal(v: Sequence[float]) -> str:
    return "[" + ",".join(f"{x:.7g}" for x in v) + "]"


def _meta(v: Any) -> Dict[str, Any]:
    if isinstance(v, str):
        try:
            v = json.loads(v)
        except ValueError:
            return {}
    return dict(v or {})


class MemoryStore:
    def __init__(self, pool: Any = None, llm: Any = None, max_items: int = 1000):
        self.pool = pool
        self.llm = llm
        self.max_items = max_items
        self.items: Dict[str, Deque[Dict[str, Any]]] = collections.defaultdict(
            lambda: collections.deque(maxlen=self.max_items))
        self._seq = 0
        self._schema: Optional[bool] = None  # None = not checked; else pgvector available?

    async def _ensure_schema(self) -> bool:
        if self._schema is None:
            vector_ok = True
            try:
                await self.pool.execute("CREATE EXTENSION IF NOT EXISTS vector")
            except Exception as err:  # noqa: BLE001 - extension may be missing or not permitted
                log.info("pgvector unavailable (%s); memory recall will use keywords", err)
            vector_ok = bool(await self.pool.fetchval("SELECT 1 FROM pg_extension WHERE extname = 'vector'"))
            await self.pool.execute(
                "CREATE TABLE IF NOT EXISTS agent_memory ("
                " id BIGSERIAL PRIMARY KEY, agent TEXT NOT NULL, text TEXT NOT NULL,"
                " metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT now())")
            await self.pool.execute(
                "CREATE INDEX IF NOT EXISTS agent_memory_agent_idx ON agent_memory (agent, id DESC)")
            if vector_ok:
                await self.pool.execute(
                    f"ALTER TABLE agent_memory ADD COLUMN IF NOT EXISTS embedding vector({EMBEDDING_DIM})")
            self._schema = vector_ok
        return self._schema

    async def _embed(self, text: str, agent: str) -> Optional[List[float]]:
        if self.llm is None:
            return None
        vec, _reason = await self.llm.embed(text, agent)
        return vec

    async def remember(self, agent: str, text: str, metadata: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        metadata = dict(metadata or {})
        vec = await self._embed(text, agent)
        if self.pool is not None:
            try:
                vector_ok = await self._ensure_schema()
                if vector_ok and vec is not None and len(vec) == EMBEDDING_DIM:
                    new_id = await self.pool.fetchval(
                        "INSERT INTO agent_memory (agent, text, metadata, embedding) "
                        "VALUES ($1, $2, $3::jsonb, $4::vector) RETURNING id",
                        agent, text, json.dumps(metadata, default=str), _vec_literal(vec))
                    return {"id": new_id, "agent": agent, "embedded": True}
                new_id = await self.pool.fetchval(
                    "INSERT INTO agent_memory (agent, text, metadata) VALUES ($1, $2, $3::jsonb) RETURNING id",
                    agent, text, json.dumps(metadata, default=str))
                return {"id": new_id, "agent": agent, "embedded": False}
            except Exception as err:  # noqa: BLE001
                log.warning("agent_memory write failed (kept in memory): %s", err)
        self._seq += 1
        self.items[agent].appendleft({"id": self._seq, "text": text, "metadata": metadata, "embedding": vec})
        return {"id": self._seq, "agent": agent, "embedded": vec is not None}

    async def recall(self, agent: str, query: str, k: int = 5) -> List[Dict[str, Any]]:
        k = max(1, int(k))
        qvec = await self._embed(query, agent)
        if self.pool is not None:
            try:
                vector_ok = await self._ensure_schema()
                if vector_ok and qvec is not None and len(qvec) == EMBEDDING_DIM:
                    rows = await self.pool.fetch(
                        "SELECT id, text, metadata, 1 - (embedding <=> $2::vector) AS score FROM agent_memory "
                        "WHERE agent = $1 AND embedding IS NOT NULL ORDER BY embedding <=> $2::vector LIMIT $3",
                        agent, _vec_literal(qvec), k)
                    if rows:
                        return [{"id": r["id"], "text": r["text"], "metadata": _meta(r["metadata"]),
                                 "score": round(float(r["score"]), 6), "method": "vector"} for r in rows]
                rows = await self.pool.fetch(
                    "SELECT id, text, metadata FROM agent_memory WHERE agent = $1 ORDER BY id DESC LIMIT $2",
                    agent, KEYWORD_SCAN_ROWS)
                return _rank_keywords(query, [{"id": r["id"], "text": r["text"], "metadata": _meta(r["metadata"])}
                                              for r in rows], k)
            except Exception as err:  # noqa: BLE001
                log.warning("agent_memory read failed (serving memory): %s", err)
        items = list(self.items.get(agent, ()))
        if qvec is not None and any(i["embedding"] for i in items):
            scored = [{"id": i["id"], "text": i["text"], "metadata": i["metadata"],
                       "score": cosine(qvec, i["embedding"]), "method": "vector"}
                      for i in items if i["embedding"]]
            scored.sort(key=lambda h: h["score"], reverse=True)
            return scored[:k]
        return _rank_keywords(query, items, k)


def _rank_keywords(query: str, items: List[Dict[str, Any]], k: int) -> List[Dict[str, Any]]:
    """Keyword-ranked hits; items are newest first and the sort is stable, so ties favour recent."""
    hits = []
    for i in items:
        score = keyword_score(query, i["text"])
        if score > 0:
            hits.append({"id": i["id"], "text": i["text"], "metadata": i["metadata"], "score": score,
                         "method": "keyword"})
    hits.sort(key=lambda h: h["score"], reverse=True)
    return hits[:k]


# ---- process-wide store ----
_store: Optional[MemoryStore] = None


async def get_memory_store() -> MemoryStore:
    global _store
    if _store is None:
        from src.agents_py.core.llm import get_llm

        _store = MemoryStore(pool=await infra.get_pool(), llm=await get_llm())
    elif _store.pool is None:  # Postgres connected after the store was built
        _store.pool = await infra.get_pool()
    return _store


def reset_memory_store() -> None:
    global _store
    _store = None


async def remember(agent: str, text: str, metadata: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    return await (await get_memory_store()).remember(agent, text, metadata)


async def recall(agent: str, query: str, k: int = 5) -> List[Dict[str, Any]]:
    return await (await get_memory_store()).recall(agent, query, k)
