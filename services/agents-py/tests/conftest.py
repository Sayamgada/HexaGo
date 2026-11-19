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
    monkeypatch.setattr(infra.settings, "SETTLEMENT_SCAN_INTERVAL_S", 0)
    monkeypatch.setattr(infra.settings, "SURGE_MAP_INTERVAL_S", 0)
    monkeypatch.setattr(infra.settings, "FORECAST_INTERVAL_S", 0)
    _reset_singletons()
    yield
    _reset_singletons()


def _reset_singletons():
    from src.agents_py.agents.forecast import agent as forecast_agent
    from src.agents_py.agents.pricing import surge_map
    from src.agents_py.core import events, llm, memory

    infra.reset()
    llm.reset_llm()
    events.reset_event_store()
    memory.reset_memory_store()
    surge_map.set_map(None)
    forecast_agent.set_agent(None)


@pytest.fixture
def redis():
    return fakeredis.FakeAsyncRedis(decode_responses=True)


@pytest.fixture
def clock():
    return FakeClock()
