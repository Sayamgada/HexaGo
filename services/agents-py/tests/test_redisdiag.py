import fakeredis
import pytest
from httpx import ASGITransport, AsyncClient

from src.agents_py.core import events as ev
from src.agents_py.core import infra
from src.agents_py.core import llm as llm_mod
from src.agents_py.core import redisdiag as rd
from src.agents_py.main import app


def test_describe_masks_password():
    info = rd.describe("rediss://red-abc:s3cr3t@oregon-keyvalue.render.com:6379/2")
    assert info == {"url": "rediss://red-abc:****@oregon-keyvalue.render.com:6379/2", "scheme": "rediss",
                    "host": "oregon-keyvalue.render.com", "port": 6379, "db": 2, "tls": True,
                    "render_internal_host": False}
    internal = rd.describe("redis://red-abc123:6379")
    assert internal["url"] == "redis://red-abc123:6379/0" and internal["render_internal_host"] is True
    assert rd.describe("redis://:pw@localhost")["url"] == "redis://:****@localhost:6379/0"
    with pytest.raises(ValueError):
        rd.describe("http://x")


@pytest.mark.parametrize("msg,word", [
    ("Error -2 connecting to red-abc:6379. Name or service not known.", "region"),
    ("Error 11001 connecting to red-abc:6379. getaddrinfo failed.", "region"),
    ("Error 111 connecting to 1.2.3.4:6379. Connection refused.", "refused"),
    ("Connection closed by server.", "TLS"),
    ("[SSL: WRONG_VERSION_NUMBER] wrong version number", "redis://"),
    ("[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed", "certificate"),
    ("WRONGPASS invalid username-password pair", "password"),
    ("Authentication required.", "password"),
    ("Timeout connecting to server", "allow list"),
    ("weird", ""),
])
def test_hint(msg, word):
    h = rd.hint(Exception(msg))
    assert (word in h) if word else h == ""


async def test_check_round_trip_ok(redis):
    res = await rd.check("redis://localhost:6379/0", client=redis)
    assert res["ok"] and res["configured"] and res["failed_stage"] is None
    assert res["ping_ms"] >= 0 and res["pubsub_roundtrip_ms"] >= 0 and res["checked_at"]


async def test_check_not_configured_bad_url_and_unreachable():
    assert (await rd.check(""))["configured"] is False
    assert (await rd.check("http://x"))["failed_stage"] == "parse"
    res = await rd.check("redis://:pw@127.0.0.1:1/0", timeout_s=1)
    assert not res["ok"] and res["failed_stage"] == "ping" and res["hint"]
    assert "pw" not in res["url"]


async def test_check_reports_publish_and_receive_failures():
    class NoPublish(fakeredis.FakeAsyncRedis):
        async def publish(self, *a, **k):
            raise ConnectionError("Connection closed by server.")

    res = await rd.check("redis://h", client=NoPublish(decode_responses=True))
    assert res["failed_stage"] == "publish" and "TLS" in res["hint"]

    class Silent(fakeredis.FakeAsyncRedis):
        async def publish(self, *a, **k):
            return 0  # message never delivered

    res = await rd.check("redis://h", client=Silent(decode_responses=True), timeout_s=0.2)
    assert res["failed_stage"] == "receive"


async def test_monitor_caches_and_fresh(redis):
    m = rd.RedisMonitor("redis://localhost:6379/0", client_factory=lambda: redis)
    assert m.last()["checked_at"] is None and m.last()["configured"]
    await m.check_now()
    assert m.last()["ok"]
    await m.check_now()  # unchanged state: no new log line, still ok
    assert m.last()["ok"]


async def test_healthz_has_redis_block(monkeypatch):
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://t") as c:
        body = (await c.get("/healthz")).json()
        assert body["redis"]["configured"] is False
        body = (await c.get("/healthz", params={"redis": "fresh"})).json()
        assert body["redis"]["configured"] is False and body["redis"]["checked_at"]


async def test_stores_pick_up_redis_after_late_connect(redis):
    """Redis down when the singletons are built, up later: they must start using it."""
    store = await ev.get_event_store()
    client = await llm_mod.get_llm()
    assert store.redis is None and client.store.redis is None
    infra.override(redis=redis, pool=None)
    store = await ev.get_event_store()
    client = await llm_mod.get_llm()
    assert store.redis is redis and client.store.redis is redis


async def test_tls_timeout_gets_tls_hint():
    class Hang(fakeredis.FakeAsyncRedis):
        async def ping(self, *a, **k):
            import asyncio
            await asyncio.sleep(5)

    res = await rd.check("rediss://red-abc:6379", client=Hang(), timeout_s=0.05)
    assert res["failed_stage"] == "ping" and "TLS" in res["hint"] and "timed out" in res["error"]
    assert "password" in rd.hint(Exception("AUTH <password> called without any password configured"))
