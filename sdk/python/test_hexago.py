"""Unit tests for the Python SDK against a local stdlib HTTP server (no network)."""
import hashlib
import hmac
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from hexago import HexaGo, HexaGoError, verify_webhook


class Handler(BaseHTTPRequestHandler):
    seen = []

    def log_message(self, *a):
        pass

    def _send(self, code, body, headers=None):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("X-RateLimit-Limit", "60")
        self.send_header("X-RateLimit-Remaining", "59")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _record(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n)) if n else None
        Handler.seen.append((self.command, self.path, self.headers.get("Authorization"), body))
        return body

    def do_POST(self):
        body = self._record()
        if self.path == "/v1/developer/keys":
            self._send(201, {"api_key": "hxg_" + "a" * 48, "scopes": body.get("scopes", [])})
        elif self.path == "/v1/rides":
            self._send(201, {"ride": {"id": "r1", "status": "matched", **body}})
        else:
            self._send(429, {"error": "slow down"}, {"Retry-After": "3"})

    def do_GET(self):
        self._record()
        if self.path.startswith("/v1/eta"):
            self._send(200, {"path": self.path})
        else:
            self._send(404, {"detail": "nope"})

    def do_DELETE(self):
        self._record()
        self._send(200, {"deleted": True})


@pytest.fixture
def server():
    Handler.seen = []
    srv = HTTPServer(("127.0.0.1", 0), Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


def test_create_key_and_request_ride(server):
    hx = HexaGo.create_key("t", scopes=["rides:write"], base_url=server)
    assert hx.api_key.startswith("hxg_")
    ride = hx.request_ride("u1", (12.9, 77.5), (13.0, 77.6), tier="sedan")
    assert ride["ride"]["pickup"] == {"lat": 12.9, "lng": 77.5} and ride["ride"]["tier"] == "sedan"
    method, path, auth, _ = Handler.seen[-1]
    assert (method, path, auth) == ("POST", "/v1/rides", "Bearer " + hx.api_key)
    assert hx.rate_limit["X-RateLimit-Remaining"] == "59"
    assert Handler.seen[0][2] is None  # key creation is keyless


def test_query_params_and_errors(server):
    hx = HexaGo(api_key="hxg_x", base_url=server + "/")
    assert hx.eta((1, 2), (3, 4))["path"] == "/v1/eta?from_lat=1&from_lng=2&to_lat=3&to_lng=4"
    with pytest.raises(HexaGoError) as e:
        hx.ride("missing")
    assert e.value.status == 404 and "nope" in str(e.value)
    with pytest.raises(HexaGoError) as e:
        hx.quote((1, 2), (3, 4))
    assert e.value.status == 429 and e.value.body["error"] == "slow down"
    assert hx.delete_webhook("wh_1") == {"deleted": True}


def test_stream_url():
    assert HexaGo(api_key="hxg_k", base_url="https://api.example").stream_url() == "wss://api.example/v1/stream?api_key=hxg_k"
    assert HexaGo(base_url="http://localhost:8080").stream_url() == "ws://localhost:8080/v1/stream"


def test_verify_webhook():
    body, ts, secret = b'{"type":"ride.completed"}', "1700000000", "whsec_test"
    sig = "sha256=" + hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    assert verify_webhook(secret, body, ts, sig)
    assert not verify_webhook(secret, body + b" ", ts, sig)
    assert not verify_webhook("other", body, ts, sig)
