"""The Safety Sentinel's hands: calls into dispatch-go's safety endpoints.

    get_ride(id)                     GET  /v1/rides/{id}
    suspend_driver(...)              POST /v1/safety/drivers/{id}/suspend
    unsuspend_driver(..., conf)      POST /v1/safety/drivers/{id}/unsuspend (Ops Supervisor confirmation only)
    create_track_link(...)           POST /v1/safety/track
    emergency_dispatch(payload)      POST /v1/safety/emergency/dispatch       (simulated emergency services)
    notify_contact(payload)          POST /v1/safety/emergency/notify-contact (simulated SMS)
    vehicle_path(driver, since)      GET  /v1/safety/vehicle-path/{driver}
    publish(topic, data)             POST /v1/safety/publish, else Redis ``hexago:stream`` (gateway wire format)
"""
from __future__ import annotations

import datetime as dt
import json
import logging
from typing import Any, Awaitable, Callable, Dict, List, Optional
from urllib.parse import quote

import httpx

from src.agents_py.agents.safety.decision import Confirmation
from src.agents_py.config import settings
from src.agents_py.core import infra
from src.agents_py.core.events import GATEWAY_CHANNEL

log = logging.getLogger("hexago.safety.actions")


class ActionError(Exception):
    pass


class SafetyActions:
    def __init__(self, base_url: Optional[str] = None, timeout_s: float = 4.0,
                 transport: Optional[httpx.AsyncBaseTransport] = None,
                 redis: Optional[Callable[[], Awaitable[Any]]] = None):
        self.base_url = (base_url or settings.DISPATCH_URL).rstrip("/")
        self._http = httpx.AsyncClient(base_url=self.base_url, timeout=timeout_s, transport=transport)
        self._redis = redis or infra.get_redis

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _request(self, method: str, path: str, body: Optional[Dict[str, Any]] = None,
                       params: Optional[Dict[str, Any]] = None) -> Optional[Dict[str, Any]]:
        try:
            resp = await self._http.request(method, path, json=body, params=params)
        except httpx.HTTPError as err:
            raise ActionError(f"{method} {path}: {type(err).__name__}: {err}") from err
        if resp.status_code == 404 and method == "GET":
            return None
        if resp.status_code >= 400:
            raise ActionError(f"{method} {path}: HTTP {resp.status_code}: {resp.text[:200]}")
        try:
            return resp.json()
        except ValueError as err:
            raise ActionError(f"{method} {path}: invalid JSON") from err

    async def get_ride(self, ride_id: str) -> Optional[Dict[str, Any]]:
        out = await self._request("GET", f"/v1/rides/{quote(ride_id, safe='')}")
        return (out or {}).get("ride") if out else None

    async def suspend_driver(self, driver_id: str, incident_id: str, reason: str) -> Dict[str, Any]:
        return await self._request("POST", f"/v1/safety/drivers/{quote(driver_id, safe='')}/suspend",
                                   {"incident_id": incident_id, "reason": reason, "actor": "safety_sentinel"}) or {}

    async def unsuspend_driver(self, driver_id: str, incident_id: str,
                               confirmation: Optional[Confirmation]) -> Dict[str, Any]:
        """Lift a suspension. Only with the Ops Supervisor's approved confirmation for this incident."""
        if confirmation is None or not confirmation.valid or confirmation.incident_id != incident_id:
            raise PermissionError("unfreezing a driver requires an approved Ops Supervisor confirmation")
        return await self._request("POST", f"/v1/safety/drivers/{quote(driver_id, safe='')}/unsuspend",
                                   {"incident_id": incident_id, "confirmed_by": confirmation.agent}) or {}

    async def create_track_link(self, driver_id: str, ride_id: str, incident_id: str) -> Dict[str, Any]:
        return await self._request("POST", "/v1/safety/track",
                                   {"driver_id": driver_id, "ride_id": ride_id, "incident_id": incident_id}) or {}

    async def emergency_dispatch(self, payload: Dict[str, Any]) -> Dict[str, Any]:
        return await self._request("POST", "/v1/safety/emergency/dispatch", payload) or {}

    async def notify_contact(self, payload: Dict[str, Any]) -> Dict[str, Any]:
        return await self._request("POST", "/v1/safety/emergency/notify-contact", payload) or {}

    async def vehicle_path(self, driver_id: str, since: Optional[dt.datetime] = None,
                           limit: int = 900) -> List[Dict[str, Any]]:
        params: Dict[str, Any] = {"limit": limit}
        if since is not None:
            params["since"] = since.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        out = await self._request("GET", f"/v1/safety/vehicle-path/{quote(driver_id, safe='')}", params=params)
        return list((out or {}).get("points") or [])

    async def publish(self, topic: str, data: Dict[str, Any]) -> str:
        """Push a frame to live-stream subscribers. Returns "gateway", "redis" or "none"."""
        try:
            await self._request("POST", "/v1/safety/publish", {"topic": topic, "data": data})
            return "gateway"
        except ActionError as err:
            log.warning("publish %s via gateway failed, trying Redis: %s", topic, err)
        try:
            redis = await self._redis()
            if redis is not None:
                frame = json.dumps({"topic": topic, "data": data}, separators=(",", ":"), default=str)
                await redis.publish(GATEWAY_CHANNEL, f"{topic}\n{frame}")
                return "redis"
        except Exception as err:  # noqa: BLE001
            log.warning("publish %s via Redis failed: %s", topic, err)
        return "none"
