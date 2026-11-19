"""dispatch-go calls the booking agent needs (DISPATCH_URL).

    GET  /v1/eta?from_lat&from_lng&to_lat&to_lng   trip estimate (road km, minutes)
    GET  /v1/supply?lat&lng                        idle vs busy cars around the pickup H3 cell
    GET  /v1/supply/snapshot?window_s              every vehicle + recent requests (surge map)
    POST /v1/scheduled                             store a confirmed plan's legs for dispatch
"""
from __future__ import annotations

from typing import Any, Dict, Optional

import httpx

from src.agents_py.agents.booking.itinerary import LatLng, TripEstimate
from src.agents_py.agents.pricing.quote import CellLoad
from src.agents_py.config import settings


class GatewayError(Exception):
    pass


class DispatchGateway:
    def __init__(self, base_url: Optional[str] = None, timeout_s: float = 3.0,
                 transport: Optional[httpx.AsyncBaseTransport] = None):
        self.base_url = (base_url or settings.DISPATCH_URL).rstrip("/")
        self.timeout_s = timeout_s
        self.transport = transport

    async def _request(self, method: str, path: str, **kw: Any) -> Dict[str, Any]:
        try:
            async with httpx.AsyncClient(base_url=self.base_url, timeout=self.timeout_s,
                                         transport=self.transport) as http:
                resp = await http.request(method, path, **kw)
        except httpx.HTTPError as err:
            raise GatewayError(f"{method} {path}: {err}") from err
        if resp.status_code >= 400:
            raise GatewayError(f"{method} {path}: HTTP {resp.status_code}: {resp.text[:200]}")
        try:
            return resp.json()
        except ValueError as err:
            raise GatewayError(f"{method} {path}: invalid JSON") from err

    async def eta(self, a: LatLng, b: LatLng) -> TripEstimate:
        d = await self._request("GET", "/v1/eta", params={
            "from_lat": a[0], "from_lng": a[1], "to_lat": b[0], "to_lng": b[1]})
        return TripEstimate(round(float(d["distance_km"]), 2), round(float(d["duration_s"]) / 60, 1), "dispatch-go")

    async def cell_load(self, lat: float, lng: float) -> CellLoad:
        """Supply/demand around the pickup; an empty load (1.0x surge) if dispatch-go is unreachable."""
        try:
            d = await self._request("GET", "/v1/supply", params={"lat": lat, "lng": lng})
        except GatewayError:
            return CellLoad(source="unavailable")
        return CellLoad(cell=str(d.get("cell", "")), supply=int(d.get("supply", 0)),
                        demand=int(d.get("demand", 0)), source="dispatch-go")

    async def snapshot(self, window_s: int = 300) -> Dict[str, Any]:
        """Vehicle positions/statuses and the ride requests of the last window_s seconds."""
        return await self._request("GET", "/v1/supply/snapshot", params={"window_s": int(window_s)})

    async def schedule(self, payload: Dict[str, Any]) -> Dict[str, Any]:
        return await self._request("POST", "/v1/scheduled", json=payload)
