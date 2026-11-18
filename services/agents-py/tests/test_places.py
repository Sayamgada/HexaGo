"""Saved places for the demo personas and the bounded, cached, rate-limited Nominatim geocoder."""
import httpx
import pytest

from src.agents_py.agents.booking.places import (
    PERSONAS, USER_AGENT, Geocoder, get_persona, haversine_km, in_city, match_saved, normalize, resolve_place,
)
from src.agents_py.core.kv import GuardStore
from tests.helpers import FakeClock, FakeSleep

CENTER = (12.9716, 77.5946)


def test_three_personas_with_real_bengaluru_places():
    assert set(PERSONAS) == {"rider_priya", "rider_arjun", "rider_meera"}
    for p in PERSONAS.values():
        keys = {pl.key for pl in p.places}
        assert {"home", "office", "airport", "soccer"} <= keys
        assert any(pl.kind == "school" for pl in p.places)
        for pl in p.places:
            assert in_city(pl.lat, pl.lng, CENTER), pl.label
        assert p.public()["saved_places"][0]["key"] == "home"
    assert get_persona("nobody").id == "rider_priya"


def test_match_saved_aliases():
    priya = PERSONAS["rider_priya"]
    assert [p.key for p in match_saved(priya, "kid's school")] == ["kid_school"]
    assert [p.key for p in match_saved(priya, "the Airport")] == ["airport"]
    assert [p.key for p in match_saved(priya, "Soccer practice")] == ["soccer"]
    assert [p.key for p in match_saved(priya, "my office in Hebbal")] == ["office"]
    assert match_saved(priya, "MG Road") == [] and match_saved(priya, "  ") == []
    arjun = PERSONAS["rider_arjun"]
    assert {p.key for p in match_saved(arjun, "school")} == {"school_aanya", "school_kabir"}
    assert [p.key for p in match_saved(arjun, "Kabir's school")] == ["school_kabir"]
    assert normalize("  to the  Kid’s School! ") == "kid's school"


async def test_resolve_place_order():
    priya = PERSONAS["rider_priya"]
    cur = await resolve_place(priya, None, None)
    assert cur.place.source == "current" and cur.place.lat == priya.home.lat
    saved = await resolve_place(priya, "office", None)
    assert saved.place.key == "office" and saved.place.source == "saved"
    arjun = PERSONAS["rider_arjun"]
    amb = await resolve_place(arjun, "school", None)
    assert amb.ambiguous and len(amb.options) == 2
    hinted = await resolve_place(arjun, "school", None, kid_hint="Kabir")
    assert hinted.place.key == "school_kabir"
    missing = await resolve_place(priya, "Narnia", None)
    assert missing.place is None and not missing.ambiguous


def nominatim(rows, status=200, calls=None):
    def handler(req: httpx.Request):
        if calls is not None:
            calls.append(req)
        return httpx.Response(status, json=rows)

    return httpx.MockTransport(handler)


async def test_geocoder_bounded_query_user_agent_and_cache(redis):
    calls = []
    clock = FakeClock(100.0)
    sleep = FakeSleep()
    store = GuardStore(redis, clock=clock)
    g = Geocoder(center=CENTER, cache=store, clock=clock, sleep=sleep,
                 transport=nominatim([{"lat": "12.9756", "lon": "77.6050", "name": "MG Road"}], calls=calls))
    place = await g.geocode("MG Road")
    assert (place.label, place.lat, place.source) == ("MG Road", 12.9756, "geocoded")
    req = calls[0]
    assert req.headers["user-agent"] == USER_AGENT
    q = req.url.params
    assert q["bounded"] == "1" and q["countrycodes"] == "in" and q["q"] == "mg road, Bengaluru"
    left, top, right, bottom = map(float, q["viewbox"].split(","))
    assert left < CENTER[1] < right and bottom < CENTER[0] < top
    # memo, then the shared (Redis) cache for a fresh geocoder
    assert (await g.geocode("mg road")).lat == 12.9756 and len(calls) == 1
    g2 = Geocoder(center=CENTER, cache=store, transport=nominatim([], calls=calls))
    assert (await g2.geocode("MG road")).label == "MG Road" and len(calls) == 1


async def test_geocoder_rate_limits_to_one_per_second():
    clock = FakeClock(100.0)
    sleep = FakeSleep()
    g = Geocoder(center=CENTER, clock=clock, sleep=sleep, transport=nominatim([]))
    await g.geocode("place one")
    clock.advance(0.25)
    await g.geocode("place two")
    assert sleep.waits == [pytest.approx(0.75)]
    clock.advance(5)
    await g.geocode("place three")
    assert len(sleep.waits) == 1 and g.requests == 3


async def test_geocoder_rejects_out_of_city_and_falls_back_to_gazetteer():
    far = Geocoder(center=CENTER, transport=nominatim([{"lat": "28.61", "lon": "77.20", "display_name": "Delhi, IN"}]))
    assert await far.geocode("connaught place") is None
    down = Geocoder(center=CENTER, transport=nominatim([], status=503))
    place = await down.geocode("Koramangala")
    assert place.source == "gazetteer" and place.label == "Koramangala"

    def boom(req):
        raise httpx.ConnectError("offline")

    offline = Geocoder(center=CENTER, transport=httpx.MockTransport(boom))
    assert (await offline.geocode("whitefield")).label == "Whitefield"
    assert await offline.geocode("unknown street") is None
    assert await offline.geocode("   ") is None
    named = Geocoder(center=CENTER, transport=nominatim([{"lat": "12.97", "lon": "77.60",
                                                          "display_name": "Brigade Road, Bengaluru"}]))
    assert (await named.geocode("brigade")).label == "Brigade Road"


async def test_geocoder_cache_failures_are_ignored():
    class Broken:
        async def get_json(self, k):
            raise ConnectionError

        async def set_json(self, k, v, ttl):
            raise ConnectionError

    g = Geocoder(center=CENTER, cache=Broken(), transport=nominatim([{"lat": "12.97", "lon": "77.6", "name": "X"}]))
    assert (await g.geocode("x place")).label == "X"


async def test_resolve_place_uses_geocoder():
    g = Geocoder(center=CENTER, transport=nominatim([{"lat": "12.93", "lon": "77.62", "name": "Forum Mall"}]))
    res = await resolve_place(PERSONAS["rider_priya"], "Forum Mall", g)
    assert res.place.label == "Forum Mall" and res.place.source == "geocoded"


def test_haversine_and_city_box():
    assert haversine_km(CENTER, CENTER) == 0
    assert 18 < haversine_km((13.0475, 77.6200), (13.1989, 77.7068)) < 20
    assert not in_city(28.6, 77.2, CENTER)
