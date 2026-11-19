"""HexaGo Python SDK: a small hand-written client for the HexaGo Public API.

Only the standard library is needed for REST calls; ``stream()`` needs
``pip install websockets``.

    from hexago import HexaGo
    hx = HexaGo(api_key="hxg_...")                    # or HexaGo.create_key()
    ride = hx.request_ride("me", (12.9716, 77.5946), (12.9352, 77.6245))

API reference: https://dispatch-go.onrender.com/docs (OpenAPI: /openapi.json).
"""
from __future__ import annotations

import hashlib
import hmac
import json
import os
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, AsyncIterator, Dict, Iterable, Optional, Tuple

__all__ = ["HexaGo", "HexaGoError", "verify_webhook", "DEFAULT_BASE_URL"]
__version__ = "1.0.0"

DEFAULT_BASE_URL = os.environ.get("HEXAGO_BASE_URL", "https://dispatch-go.onrender.com")
LatLng = Tuple[float, float]


class HexaGoError(Exception):
    """A non-2xx answer. ``status`` is the HTTP status, ``body`` the decoded JSON (if any)."""

    def __init__(self, status: int, body: Any, rate_limit: Dict[str, str]):
        self.status, self.body, self.rate_limit = status, body, rate_limit
        msg = body.get("error") or body.get("detail") if isinstance(body, dict) else body
        super().__init__(f"HTTP {status}: {msg}")


def _point(p: LatLng) -> Dict[str, float]:
    return {"lat": float(p[0]), "lng": float(p[1])}


class HexaGo:
    """Client for the HexaGo gateway. Every call returns the decoded JSON body."""

    def __init__(self, api_key: Optional[str] = None, base_url: str = DEFAULT_BASE_URL, timeout: float = 30.0):
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.rate_limit: Dict[str, str] = {}  # X-RateLimit-* of the last response

    # ---- transport ----

    def request(self, method: str, path: str, body: Any = None, params: Optional[Dict[str, Any]] = None) -> Any:
        url = self.base_url + path
        if params:
            url += "?" + urllib.parse.urlencode({k: v for k, v in params.items() if v is not None})
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", f"hexago-python/{__version__}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if self.api_key:
            req.add_header("Authorization", f"Bearer {self.api_key}")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                self._remember(resp.headers)
                raw = resp.read()
        except urllib.error.HTTPError as err:
            self._remember(err.headers)
            raw = err.read()
            try:
                decoded = json.loads(raw)
            except ValueError:
                decoded = raw.decode(errors="replace")
            raise HexaGoError(err.code, decoded, dict(self.rate_limit)) from None
        return json.loads(raw) if raw else None

    def _remember(self, headers) -> None:
        self.rate_limit = {k: v for k, v in (headers or {}).items() if k.lower().startswith("x-ratelimit-")}

    # ---- developer platform ----

    @classmethod
    def create_key(cls, name: str = "python sdk", scopes: Optional[Iterable[str]] = None,
                   base_url: str = DEFAULT_BASE_URL, **kw) -> "HexaGo":
        """Create a demo API key and return a client that uses it (the key is in ``client.api_key``)."""
        body: Dict[str, Any] = {"name": name}
        if scopes:
            body["scopes"] = list(scopes)
        created = cls(base_url=base_url, **kw).request("POST", "/v1/developer/keys", body)
        return cls(api_key=created["api_key"], base_url=base_url, **kw)

    def me(self) -> Dict[str, Any]:
        return self.request("GET", "/v1/developer/keys/me")

    def create_webhook(self, url: str, events: Iterable[str]) -> Dict[str, Any]:
        """Subscribe ``url`` to ride.completed / incident.opened / payout.sent; returns the signing secret once."""
        return self.request("POST", "/v1/developer/webhooks", {"url": url, "events": list(events)})

    def webhooks(self) -> Dict[str, Any]:
        return self.request("GET", "/v1/developer/webhooks")

    def delete_webhook(self, webhook_id: str) -> Dict[str, Any]:
        return self.request("DELETE", f"/v1/developer/webhooks/{webhook_id}")

    # ---- rides ----

    def request_ride(self, rider_id: str, pickup: LatLng, dropoff: LatLng, tier: str = "mini") -> Dict[str, Any]:
        """Request a ride; ``result["ride"]["id"]`` identifies it, ``result["match"]`` is set when a driver was found."""
        return self.request("POST", "/v1/rides", {"rider_id": rider_id, "pickup": _point(pickup),
                                                   "dropoff": _point(dropoff), "tier": tier})

    def ride(self, ride_id: str) -> Dict[str, Any]:
        return self.request("GET", f"/v1/rides/{ride_id}")

    def cancel_ride(self, ride_id: str, reason: str = "") -> Dict[str, Any]:
        return self.request("POST", f"/v1/rides/{ride_id}/cancel", {"reason": reason})

    def eta(self, origin: LatLng, destination: LatLng) -> Dict[str, Any]:
        return self.request("GET", "/v1/eta", params={"from_lat": origin[0], "from_lng": origin[1],
                                                      "to_lat": destination[0], "to_lng": destination[1]})

    # ---- booking agent, pricing ----

    def book(self, text: str, rider_id: str = "rider_priya") -> Dict[str, Any]:
        """Natural-language booking: returns a priced plan (``type == "plan"``) or a clarifying question."""
        return self.request("POST", "/v1/agents/booking/parse", {"rider_id": rider_id, "text": text})

    def confirm(self, plan_id: str) -> Dict[str, Any]:
        return self.request("POST", "/v1/agents/booking/confirm", {"plan_id": plan_id})

    def quote(self, pickup: LatLng, dropoff: LatLng, tier: str = "mini") -> Dict[str, Any]:
        return self.request("POST", "/v1/pricing/quote", {"tier": tier, "pickup": _point(pickup), "dropoff": _point(dropoff)})

    # ---- safety, ledger, agents ----

    def incidents(self) -> Dict[str, Any]:
        return self.request("GET", "/v1/safety/incidents")

    def wallet(self, owner: str) -> Dict[str, Any]:
        return self.request("GET", f"/v1/wallets/{owner}")

    def ride_ledger(self, ride_id: str) -> Dict[str, Any]:
        return self.request("GET", "/v1/ledger/entries", params={"ride_id": ride_id})

    def agents(self) -> Dict[str, Any]:
        return self.request("GET", "/v1/agents")

    # ---- live stream ----

    def stream_url(self) -> str:
        ws = "wss://" + self.base_url[len("https://"):] if self.base_url.startswith("https://") else \
            "ws://" + self.base_url.split("://", 1)[-1]
        q = "?" + urllib.parse.urlencode({"api_key": self.api_key}) if self.api_key else ""
        return f"{ws}/v1/stream{q}"

    async def stream(self, *topics: str) -> AsyncIterator[Dict[str, Any]]:
        """Yield ``{"topic", "data"}`` frames for topics such as ``"ride:<id>"``, ``"drivers"``, ``"ops"``."""
        import websockets  # optional dependency: pip install websockets

        async with websockets.connect(self.stream_url(), max_size=8 << 20) as ws:
            await ws.send(json.dumps({"action": "subscribe", "topics": list(topics)}))
            async for raw in ws:
                yield json.loads(raw)


def verify_webhook(secret: str, body: bytes, timestamp: str, signature: str) -> bool:
    """Check ``X-HexaGo-Signature`` (``sha256=`` + hex HMAC-SHA256 of ``timestamp + "." + body``)."""
    mac = hmac.new(secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest("sha256=" + mac, signature)
