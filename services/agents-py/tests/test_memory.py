import json

from src.agents_py.core import infra
from src.agents_py.core import memory as mem
from tests.helpers import FakePool, ScriptedProvider, embedding_response, make_llm


def test_keyword_score():
    assert mem.keyword_score("flooded underpass Silk Board", "Silk Board junction flooding at 6pm") == 0.75
    assert mem.keyword_score("silk board flooding", "MG Road clear") == 0.0
    assert mem.keyword_score("", "anything") == 0.0


def test_cosine():
    assert mem.cosine([1, 0], [1, 0]) == 1.0
    assert mem.cosine([1, 0], [0, 1]) == 0.0
    assert mem.cosine([0, 0], [1, 1]) == 0.0


async def test_keyword_fallback_in_memory_when_llm_disabled(redis):
    store = mem.MemoryStore(pool=None, llm=make_llm(redis, enabled=False))
    await store.remember("safety_sentinel", "Silk Board junction floods after heavy rain", {"zone": "hsr"})
    await store.remember("safety_sentinel", "Driver d42 hard-braked near Hebbal flyover")
    await store.remember("surge_equilibrium", "Silk Board surge 1.8x during rain")
    hits = await store.recall("safety_sentinel", "rain at silk board", k=5)
    assert [h["text"] for h in hits] == ["Silk Board junction floods after heavy rain"]
    assert hits[0]["metadata"] == {"zone": "hsr"} and hits[0]["method"] == "keyword"
    assert hits[0]["score"] > 0


async def test_vector_recall_in_memory(redis):
    vectors = {
        "flood at silk board": [1.0, 0.0, 0.0],
        "hebbal hard brake": [0.0, 1.0, 0.0],
        "waterlogging near hsr": [0.9, 0.1, 0.0],
    }

    async def fake_embed(**kw):
        return embedding_response(vectors[kw["input"][0]])

    llm = make_llm(redis, embedding=ScriptedProvider([fake_embed] * 3), min_interval_s=0)
    store = mem.MemoryStore(pool=None, llm=llm)
    await store.remember("safety", "flood at silk board")
    await store.remember("safety", "hebbal hard brake")
    hits = await store.recall("safety", "waterlogging near hsr", k=1)
    assert hits[0]["text"] == "flood at silk board" and hits[0]["method"] == "vector"


async def test_in_memory_list_is_bounded():
    store = mem.MemoryStore(pool=None, llm=None, max_items=3)
    for i in range(5):
        await store.remember("a", f"note {i}")
    assert len(store.items["a"]) == 3
    assert await store.recall("a", "", k=2) == []


def _pg_handler(vector_ok=True, rows=None):
    def handler(sql, args):
        if "CREATE EXTENSION" in sql and not vector_ok:
            raise RuntimeError("permission denied to create extension")
        if "SELECT 1 FROM pg_extension" in sql:
            return 1 if vector_ok else None
        if "RETURNING id" in sql:
            return 7
        if sql.strip().startswith("SELECT"):
            return rows or []
        return None
    return handler


async def test_pgvector_path(redis):
    rows = [{"id": 1, "text": "flood at silk board", "metadata": json.dumps({"z": 1}), "score": 0.93}]
    pool = FakePool(_pg_handler(True, rows))
    llm = make_llm(redis, embedding=ScriptedProvider([embedding_response([0.5] * 768)] * 2), min_interval_s=0)
    store = mem.MemoryStore(pool=pool, llm=llm)
    out = await store.remember("safety", "flood at silk board", {"z": 1})
    assert out["id"] == 7 and out["embedded"] is True
    insert = next(a for k, s, a in pool.log if s.startswith("INSERT INTO agent_memory"))
    assert insert[0] == "safety" and insert[3].startswith("[0.5,")
    hits = await store.recall("safety", "flooding", k=3)
    assert hits == [{"id": 1, "text": "flood at silk board", "metadata": {"z": 1}, "score": 0.93,
                     "method": "vector"}]
    assert any("<=>" in s for s in pool.sql())


async def test_pg_without_vector_extension_uses_keywords(redis):
    rows = [{"id": 1, "text": "Silk Board floods", "metadata": {}},
            {"id": 2, "text": "MG Road clear", "metadata": "{}"}]
    pool = FakePool(_pg_handler(False, rows))
    store = mem.MemoryStore(pool=pool, llm=make_llm(redis, enabled=False))
    out = await store.remember("safety", "Silk Board floods")
    assert out["embedded"] is False
    insert = next(a for k, s, a in pool.log if s.startswith("INSERT INTO agent_memory"))
    assert len(insert) == 3  # no embedding column
    hits = await store.recall("safety", "silk board", k=5)
    assert [h["id"] for h in hits] == [1] and hits[0]["method"] == "keyword"
    assert not any("<=>" in s for s in pool.sql())


async def test_embedding_dimension_mismatch_is_not_stored(redis):
    pool = FakePool(_pg_handler(True))
    llm = make_llm(redis, embedding=ScriptedProvider([embedding_response([0.1, 0.2])]))
    store = mem.MemoryStore(pool=pool, llm=llm)
    out = await store.remember("a", "short vector")
    assert out["embedded"] is False


async def test_database_failure_falls_back_to_memory(redis):
    store = mem.MemoryStore(pool=FakePool(fail=True), llm=make_llm(redis, enabled=False))
    await store.remember("a", "silk board flood")
    hits = await store.recall("a", "silk board", k=1)
    assert hits[0]["text"] == "silk board flood"


async def test_module_level_helpers(redis):
    infra.override(redis=redis, pool=None)
    mem.reset_memory_store()
    await mem.remember("ops_supervisor", "fallback rate spiked on surge agent")
    hits = await mem.recall("ops_supervisor", "surge fallback", k=1)
    assert hits and hits[0]["text"].startswith("fallback rate")
    mem.reset_memory_store()
