"""Shared fixtures: isolate tests from real infra; fakeredis; fake clock."""
import fakeredis
import pytest

from src.agents_py.core import infra
from tests.helpers import FakeClock


@pytest.fixture(autouse=True)
def _no_external_infra(monkeypatch):
    """Never touch real Redis/Postgres/LLMs unless a test wires them in explicitly."""
    monkeypatch.setattr(infra.settings, "REDIS_URL", "")
    monkeypatch.setattr(infra.settings, "DATABASE_URL", "")
    monkeypatch.setattr(infra.settings, "GEMINI_API_KEY", None)
    monkeypatch.setattr(infra.settings, "HEARTBEAT_INTERVAL_S", 0)
    _reset_singletons()
    yield
    _reset_singletons()


def _reset_singletons():
    from src.agents_py.core import events, llm, memory

    infra.reset()
    llm.reset_llm()
    events.reset_event_store()
    memory.reset_memory_store()


@pytest.fixture
def redis():
    return fakeredis.FakeAsyncRedis(decode_responses=True)


@pytest.fixture
def clock():
    return FakeClock()
