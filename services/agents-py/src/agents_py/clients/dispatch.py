"""Async client for dispatch-go (DISPATCH_URL).

* ``get_ride(id)``         -> GET /v1/rides/{id} (None on 404)
* ``get_metrics()``        -> GET /v1/metrics/dispatch
* ``list_drivers_near()``  -> GEOSEARCH on the ``fleet:drivers:geo`` Redis set that
  dispatch-go mirrors driver positions into "so other services can run GEOSEARCH
  without calling the gateway" (dispatch-go has no REST endpoint for this).
  Returns [] when Redis is not available.
"""
from __future__ import annotations

import logging
from typing import Any, Dict, List, Optional

import httpx

from src.agents_py.config import settings

log = logging.getLogger("hexago.dispatch_client")

DRIVERS_GEO_KEY = "fleet:drivers:geo"  # dispatch-go fleet.RedisGeoKey


class DispatchError(Exception):
    pass


class DispatchClient:
    def __init__(self, base_url: Optional[str] = None, redis: Any = None, timeout_s: float = 5.0,
                 transport: Optional[httpx.AsyncBaseTransport] = None):
        self.base_url = (base_url or settings.DISPATCH_URL).rstrip("/")
        self.redis = redis
        self._http = httpx.AsyncClient(base_url=self.base_url, timeout=timeout_s, transport=transport)

    async def __aenter__(self) -> "DispatchClient":
        return self

    async def __aexit__(self, *exc) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _get(self, path: str) -> Optional[Dict[str, Any]]:
        try:
            resp = await self._http.get(path)
        except httpx.HTTPError as err:
            raise DispatchError(f"GET {path}: {err}") from err
        if resp.status_code == 404:
            return None
        if resp.status_code >= 400:
            raise DispatchError(f"GET {path}: HTTP {resp.status_code}: {resp.text[:200]}")
        return resp.json()

    async def get_ride(self, ride_id: str) -> Optional[Dict[str, Any]]:
        """The ride view ({"ride": ..., "match": ...}) or None if it does not exist."""
        return await self._get(f"/v1/rides/{ride_id}")

    async def get_metrics(self) -> Dict[str, Any]:
        """Dispatch metrics: p50/p95/p99 latency, matches, active/idle drivers, open requests."""
        return await self._get("/v1/metrics/dispatch") or {}

    async def list_drivers_near(self, lat: float, lng: float, radius_m: float = 3000,
                                limit: int = 50) -> List[Dict[str, Any]]:
        """Drivers within radius_m of (lat, lng), nearest first: [{id, lat, lng, distance_m}]."""
        if self.redis is None:
            return []
        try:
            rows = await self.redis.geosearch(
                DRIVERS_GEO_KEY, longitude=lng, latitude=lat, radius=radius_m, unit="m",
                sort="ASC", count=limit, withcoord=True, withdist=True)
        except Exception as err:  # noqa: BLE001
            log.warning("geosearch %s failed: %s", DRIVERS_GEO_KEY, err)
            return []
        out = []
        for name, dist, (lon, la) in rows:
            out.append({"id": name, "lat": round(float(la), 6), "lng": round(float(lon), 6),
                        "distance_m": round(float(dist), 1)})
        return out
