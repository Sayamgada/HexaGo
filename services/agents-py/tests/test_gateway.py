"""dispatch-go client used by the booking agent: ETA, cell supply/demand, scheduling."""
import httpx
import pytest

from src.agents_py.agents.booking.gateway import DispatchGateway, GatewayError


def gw(handler):
    return DispatchGateway("http://dispatch/", transport=httpx.MockTransport(handler))


async def test_eta_supply_and_schedule():
    seen = []

    def handler(req: httpx.Request):
        seen.append((req.method, req.url.path, dict(req.url.params)))
        if req.url.path == "/v1/eta":
            return httpx.Response(200, json={"distance_km": 25.04, "duration_s": 2970})
        if req.url.path == "/v1/supply":
            return httpx.Response(200, json={"cell": "8961", "supply": 3, "demand": 9})
        return httpx.Response(201, json={"plan_id": "p1", "legs": [{"id": "sch_1"}]})

    g = gw(handler)
    trip = await g.eta((13.0, 77.6), (13.2, 77.7))
    assert (trip.distance_km, trip.duration_min, trip.source) == (25.04, 49.5, "dispatch-go")
    load = await g.cell_load(12.9, 77.6)
    assert (load.cell, load.supply, load.demand, load.source) == ("8961", 3, 9, "dispatch-go")
    assert (await g.schedule({"plan_id": "p1"}))["legs"][0]["id"] == "sch_1"
    assert seen[0][2] == {"from_lat": "13.0", "from_lng": "77.6", "to_lat": "13.2", "to_lng": "77.7"}
    assert seen[2][:2] == ("POST", "/v1/scheduled")


async def test_errors():
    g = gw(lambda req: httpx.Response(500, text="boom"))
    with pytest.raises(GatewayError):
        await g.eta((1, 2), (3, 4))
    load = await g.cell_load(1, 2)
    assert load.source == "unavailable" and load.supply == 0

    def down(req):
        raise httpx.ConnectError("refused")

    with pytest.raises(GatewayError):
        await gw(down).schedule({})
    with pytest.raises(GatewayError):
        await gw(lambda req: httpx.Response(200, text="not json")).schedule({})
