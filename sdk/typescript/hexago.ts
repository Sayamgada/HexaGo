/**
 * HexaGo TypeScript SDK: a small hand-written client for the HexaGo Public API.
 * Works in browsers and Node 22+ (global fetch, WebSocket and Web Crypto); no dependencies.
 *
 *   const hx = await HexaGo.createKey("my app");      // or new HexaGo({ apiKey: "hxg_..." })
 *   const { ride } = await hx.requestRide("me", [12.9716, 77.5946], [12.9352, 77.6245]);
 *
 * API reference: https://dispatch-go.onrender.com/docs (OpenAPI: /openapi.json).
 */

export const DEFAULT_BASE_URL: string =
  (globalThis as any).process?.env?.HEXAGO_BASE_URL ?? "https://dispatch-go.onrender.com";
export const VERSION = "1.0.0";

export type LatLng = [lat: number, lng: number];
export type Json = Record<string, any>;
export type Scope = "rides:read" | "rides:write" | "pricing:read" | "stream:read" | "agents:invoke" | "ledger:read";
export type WebhookEvent = "ride.completed" | "incident.opened" | "payout.sent";

export interface StreamFrame<T = any> {
  topic: string;
  data: T;
}

export interface RideUpdate {
  ride_id: string;
  status: "accepted" | "arriving" | "on_trip" | "completed" | "cancelled";
  driver: { id: string; lat: number; lng: number; heading: number; speed_kmh: number; tier: string };
  eta_sec: number;
}

export class HexaGoError extends Error {
  status: number;
  body: any;
  rateLimit: Record<string, string>;

  constructor(status: number, body: any, rateLimit: Record<string, string>) {
    super(`HTTP ${status}: ${body?.error ?? body?.detail ?? JSON.stringify(body)}`);
    this.status = status;
    this.body = body;
    this.rateLimit = rateLimit;
  }
}

export interface Options {
  apiKey?: string;
  baseUrl?: string;
}

const point = ([lat, lng]: LatLng) => ({ lat, lng });

export class HexaGo {
  apiKey?: string;
  baseUrl: string;
  /** X-RateLimit-* headers of the last response. */
  rateLimit: Record<string, string> = {};

  constructor(opts: Options = {}) {
    this.apiKey = opts.apiKey;
    this.baseUrl = (opts.baseUrl ?? DEFAULT_BASE_URL).replace(/\/+$/, "");
  }

  async request<T = Json>(method: string, path: string, body?: unknown, params?: Record<string, unknown>): Promise<T> {
    let url = this.baseUrl + path;
    if (params) {
      const q = new URLSearchParams();
      for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null) q.set(k, String(v));
      url += "?" + q.toString();
    }
    const headers: Record<string, string> = { Accept: "application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (this.apiKey) headers["Authorization"] = `Bearer ${this.apiKey}`;
    const res = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    this.rateLimit = {};
    res.headers.forEach((v, k) => {
      if (k.toLowerCase().startsWith("x-ratelimit-")) this.rateLimit[k.toLowerCase()] = v;
    });
    const text = await res.text();
    let data: any = text;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      /* not JSON */
    }
    if (!res.ok) throw new HexaGoError(res.status, data, { ...this.rateLimit });
    return data as T;
  }

  // ---- developer platform ----

  /** Create a demo API key and return a client that uses it. */
  static async createKey(name = "typescript sdk", scopes?: Scope[], baseUrl = DEFAULT_BASE_URL): Promise<HexaGo> {
    const created = await new HexaGo({ baseUrl }).request<Json>("POST", "/v1/developer/keys", { name, ...(scopes ? { scopes } : {}) });
    return new HexaGo({ apiKey: created.api_key, baseUrl });
  }

  me() {
    return this.request("GET", "/v1/developer/keys/me");
  }

  /** Subscribe a URL to events; the response carries the signing secret (shown once). */
  createWebhook(url: string, events: WebhookEvent[]) {
    return this.request("POST", "/v1/developer/webhooks", { url, events });
  }

  webhooks() {
    return this.request("GET", "/v1/developer/webhooks");
  }

  deleteWebhook(id: string) {
    return this.request("DELETE", `/v1/developer/webhooks/${encodeURIComponent(id)}`);
  }

  // ---- rides ----

  requestRide(riderId: string, pickup: LatLng, dropoff: LatLng, tier = "mini") {
    return this.request<{ ride: Json; match?: Json }>("POST", "/v1/rides", {
      rider_id: riderId, pickup: point(pickup), dropoff: point(dropoff), tier,
    });
  }

  ride(id: string) {
    return this.request<{ ride: Json; match?: Json }>("GET", `/v1/rides/${encodeURIComponent(id)}`);
  }

  cancelRide(id: string, reason = "") {
    return this.request("POST", `/v1/rides/${encodeURIComponent(id)}/cancel`, { reason });
  }

  eta(from: LatLng, to: LatLng) {
    return this.request("GET", "/v1/eta", undefined, { from_lat: from[0], from_lng: from[1], to_lat: to[0], to_lng: to[1] });
  }

  // ---- booking agent, pricing ----

  /** Natural-language booking: a priced plan (type "plan") or a clarifying question. */
  book(text: string, riderId = "rider_priya") {
    return this.request("POST", "/v1/agents/booking/parse", { rider_id: riderId, text });
  }

  confirm(planId: string) {
    return this.request("POST", "/v1/agents/booking/confirm", { plan_id: planId });
  }

  quote(pickup: LatLng, dropoff: LatLng, tier = "mini") {
    return this.request("POST", "/v1/pricing/quote", { tier, pickup: point(pickup), dropoff: point(dropoff) });
  }

  // ---- safety, ledger, agents ----

  incidents() {
    return this.request("GET", "/v1/safety/incidents");
  }

  wallet(owner: string) {
    return this.request("GET", `/v1/wallets/${encodeURIComponent(owner)}`);
  }

  rideLedger(rideId: string) {
    return this.request("GET", "/v1/ledger/entries", undefined, { ride_id: rideId });
  }

  agents() {
    return this.request("GET", "/v1/agents");
  }

  // ---- live stream ----

  streamUrl(): string {
    const ws = this.baseUrl.replace(/^http/, "ws");
    return `${ws}/v1/stream${this.apiKey ? `?api_key=${encodeURIComponent(this.apiKey)}` : ""}`;
  }

  /** Async iterator over `{topic, data}` frames for topics like `ride:<id>`, `drivers`, `ops`. */
  async *stream<T = any>(...topics: string[]): AsyncGenerator<StreamFrame<T>> {
    const ws = new WebSocket(this.streamUrl());
    const queue: StreamFrame<T>[] = [];
    let wake: (() => void) | null = null;
    let closed = false;
    let failure: unknown = null;
    const notify = () => {
      wake?.();
      wake = null;
    };
    ws.onopen = () => ws.send(JSON.stringify({ action: "subscribe", topics }));
    ws.onmessage = (ev) => {
      queue.push(JSON.parse(String(ev.data)));
      notify();
    };
    ws.onerror = (ev) => {
      failure = ev;
      notify();
    };
    ws.onclose = () => {
      closed = true;
      notify();
    };
    try {
      while (true) {
        if (queue.length) {
          yield queue.shift()!;
          continue;
        }
        if (failure) throw new Error("HexaGo stream error");
        if (closed) return;
        await new Promise<void>((r) => (wake = r));
      }
    } finally {
      ws.close();
    }
  }
}

/** Check X-HexaGo-Signature: "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)). */
export async function verifyWebhook(secret: string, body: string, timestamp: string, signature: string): Promise<boolean> {
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, enc.encode(`${timestamp}.${body}`)));
  const expected = "sha256=" + Array.from(mac, (b) => b.toString(16).padStart(2, "0")).join("");
  if (expected.length !== signature.length) return false;
  let diff = 0;
  for (let i = 0; i < expected.length; i++) diff |= expected.charCodeAt(i) ^ signature.charCodeAt(i);
  return diff === 0;
}
