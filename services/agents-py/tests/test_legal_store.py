"""Legal & Jurisdiction store: seeded memory working set, Postgres write-through and read-back."""
import datetime as dt
import json

from src.agents_py.agents.legal.legal_store import LegalStore
from tests.helpers import FakePool

NOW = dt.datetime(2026, 11, 20, 6, 0, tzinfo=dt.timezone.utc)


async def test_memory_store_seeds_three_cities_and_updates():
    st = LegalStore(clock=lambda: NOW)
    assert st.backend == "memory"
    cities = [j["city"] for j in await st.list()]
    assert cities == ["bengaluru", "delhi", "mumbai"]
    blr = await st.get("bengaluru")
    assert blr["version"] == 1 and blr["rules"]["max_surge_multiplier"] == 2.5
    blr["rules"]["max_surge_multiplier"] = 9  # callers get copies
    assert (await st.get("bengaluru"))["rules"]["max_surge_multiplier"] == 2.5
    assert await st.get("paris") is None

    new_rules = {**(await st.get("bengaluru"))["rules"], "max_surge_multiplier": 1.5}
    up = await st.update_rules("bengaluru", new_rules, by="legal_jurisdiction")
    assert up["version"] == 2 and up["updated_by"] == "legal_jurisdiction" and up["updated_at"] == "2026-11-20T06:00:00Z"
    assert (await st.get("bengaluru"))["rules"]["max_surge_multiplier"] == 1.5

    a1 = await st.add_audit({"city": "bengaluru", "diff": [{"field": "x"}]})
    a2 = await st.add_audit({"city": "mumbai", "diff": []})
    assert a1["id"].startswith("lga_") and a1["created_at"] == "2026-11-20T06:00:00Z"
    assert [a["id"] for a in await st.audit()] == [a2["id"], a1["id"]]
    assert [a["id"] for a in await st.audit(city="bengaluru")] == [a1["id"]]
    assert len(await st.audit(limit=1)) == 1


async def test_postgres_creates_schema_seeds_and_writes_through():
    rows = {}

    def handler(sql, args):
        if "to_regclass" in sql:
            return None
        if sql.strip().startswith("SELECT") and "FROM jurisdictions" in sql:
            return list(rows.values())
        if "INSERT INTO jurisdictions" in sql and "DO UPDATE" in sql:
            rows[args[0]] = {"city": args[0], "name": args[1], "state": args[2], "authority": args[3],
                             "rules": args[4], "version": args[5], "updated_at": args[6], "updated_by": args[7]}
        if sql.strip().startswith("SELECT") and "FROM legal_audit" in sql:
            return [{"entry": json.dumps({"id": "lga_1", "city": "delhi", "created_at": "2026-11-20T05:00:00Z"})}]
        return None

    pool = FakePool(handler)
    st = LegalStore(pool=pool, clock=lambda: NOW)
    assert st.backend == "postgres"
    await st.list()
    sql = " ".join(pool.sql())
    assert "CREATE TABLE IF NOT EXISTS jurisdictions" in sql and "CREATE TABLE IF NOT EXISTS legal_audit" in sql
    assert sql.count("ON CONFLICT (city) DO NOTHING") == 3  # seeds never overwrite live rules

    up = await st.update_rules("delhi", {**(await st.get("delhi"))["rules"], "max_driving_hours": 10}, by="x")
    assert up["version"] == 2
    assert json.loads(rows["delhi"]["rules"])["max_driving_hours"] == 10
    # read-back from Postgres (e.g. after a restart) wins over the seed
    fresh = LegalStore(pool=FakePool(handler), clock=lambda: NOW)
    got = await fresh.get("delhi")
    assert got["rules"]["max_driving_hours"] == 10 and got["version"] == 2

    await st.add_audit({"city": "delhi", "diff": []})
    assert any("INSERT INTO legal_audit" in s for s in pool.sql())
    assert (await fresh.audit())[0]["id"] == "lga_1"


async def test_postgres_failure_serves_memory():
    st = LegalStore(pool=FakePool(fail=True), clock=lambda: NOW)
    assert len(await st.list()) == 3
    up = await st.update_rules("mumbai", {**(await st.get("mumbai"))["rules"], "city_levy_pct": 3.0}, by="x")
    assert up["rules"]["city_levy_pct"] == 3.0
    assert (await st.get("mumbai"))["rules"]["city_levy_pct"] == 3.0
    await st.add_audit({"city": "mumbai"})
    assert len(await st.audit()) == 1


async def test_update_unknown_city_raises():
    st = LegalStore()
    try:
        await st.update_rules("paris", {}, by="x")
    except KeyError:
        pass
    else:
        raise AssertionError("expected KeyError")


def test_helpers():
    from src.agents_py.agents.legal import legal_store as ls

    assert ls._iso("x") == "x" and ls._iso(NOW) == "2026-11-20T06:00:00Z"
    assert ls._loads("{bad") == {} and ls._loads(None) == {} and ls._loads({"a": 1}) == {"a": 1}


async def test_audit_read_failure_serves_memory():
    st = LegalStore(pool=FakePool(fail=True), clock=lambda: NOW)
    await st.add_audit({"city": "delhi"})
    assert [a["city"] for a in await st.audit(city="delhi")] == ["delhi"]
