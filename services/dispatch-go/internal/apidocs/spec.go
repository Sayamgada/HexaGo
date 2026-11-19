package apidocs

import (
	"encoding/json"
	"net/http"
	"sync"
)

// Version is the API version reported in the document.
const Version = "1.0.0"

// Spec returns the OpenAPI 3.1 document.
func Spec() M {
	rateHeaders := M{
		"X-RateLimit-Limit":     M{"description": "Requests per minute allowed for the key", "schema": M{"type": "integer"}},
		"X-RateLimit-Remaining": M{"description": "Requests left in the bucket", "schema": M{"type": "integer"}},
		"X-RateLimit-Reset":     M{"description": "Seconds until the bucket is full again", "schema": M{"type": "integer"}},
	}
	errBody := M{"application/json": M{"schema": ref("Error")}}
	webhook := func(event, summary string) M {
		return M{"post": M{
			"summary": summary, "operationId": "webhook_" + event,
			"parameters": []M{
				{"name": "X-HexaGo-Signature", "in": "header", "required": true, "schema": M{"type": "string"},
					"description": "sha256=hex(HMAC-SHA256(secret, X-HexaGo-Timestamp + \".\" + raw body))"},
				{"name": "X-HexaGo-Timestamp", "in": "header", "required": true, "schema": M{"type": "string"}, "description": "Unix seconds"},
				{"name": "X-HexaGo-Event", "in": "header", "required": true, "schema": M{"type": "string"}},
				{"name": "X-HexaGo-Delivery", "in": "header", "required": true, "schema": M{"type": "string"}, "description": "Event id"},
			},
			"requestBody": M{"required": true, "content": M{"application/json": M{"schema": ref("WebhookPayload")}}},
			"responses":   M{"2XX": M{"description": "Accepted; anything else is retried up to 3 times (1 s, 2 s, 4 s)"}},
		}}
	}
	return M{
		"openapi":           "3.1.0",
		"jsonSchemaDialect": "https://spec.openapis.org/oas/3.1/dialect/base",
		"info": M{
			"title":   "HexaGo Public API",
			"version": Version,
			"summary": "AI-native ride hailing: rides, live stream, booking agent, pricing, safety, ledger and the agent swarm.",
			"description": "One gateway (dispatch-go) serves every endpoint; `/v1/agents/*`, `/v1/pricing/*` and the Safety Sentinel's " +
				"`/v1/safety/*` endpoints are proxied to agents-py (marked `x-served-by: agents-py`).\n\n" +
				"**Auth.** Create a key with `POST /v1/developer/keys` and send it as `Authorization: Bearer hxg_…` " +
				"(or `X-API-Key`, or `?api_key=` on WebSockets). Each operation lists the scope it needs in `x-required-scope`. " +
				"Keyed requests are rate limited per key (token bucket, 60/min by default) and answer with `X-RateLimit-*` headers. " +
				"Requests without a key are the first-party path (the HexaGo apps and internal services); a keyless browser request " +
				"from an origin outside the CORS allow-list gets 401.\n\n" +
				"Money is in integer paise (1 INR = 100 paise) on the ledger and in rupees on quotes.",
			"license": M{"name": "Proprietary (demo)", "identifier": "LicenseRef-HexaGo-Demo"},
		},
		"servers": []M{
			{"url": "https://dispatch-go.onrender.com", "description": "Production gateway (Render)"},
			{"url": "http://localhost:8080", "description": "Local dev"},
		},
		"security": []M{{}, {"bearerAuth": []string{}}, {"apiKeyHeader": []string{}}},
		"tags": []M{
			{"name": tagRides, "description": "Ride requests, drivers, ETAs, supply and scheduled legs"},
			{"name": tagStream, "description": "Live WebSocket stream"},
			{"name": tagBooking, "description": "Booking Intent agent (natural-language booking)"},
			{"name": tagPricing, "description": "Fare quotes and the rate card"},
			{"name": tagSafety, "description": "Safety Sentinel: signals, incidents, check-ins, suspensions"},
			{"name": tagLedger, "description": "Double-entry clearing ledger, wallets and payouts"},
			{"name": tagAgents, "description": "The 11-agent swarm: roster, audit events, LLM budget, settlement"},
			{"name": tagSim, "description": "Fleet simulator (DEMO_MODE)"},
			{"name": tagDeveloper, "description": "API keys and webhooks"},
			{"name": tagSystem, "description": "Probes, metrics and documentation"},
		},
		"paths": paths(),
		"webhooks": M{
			"ride.completed":  webhook("ride_completed", "A ride was completed (data: Ride)"),
			"incident.opened": webhook("incident_opened", "The Safety Sentinel opened an incident (data: incident summary)"),
			"payout.sent":     webhook("payout_sent", "A driver / operator payout left the ledger (data: Payout)"),
		},
		"components": M{
			"schemas": schemas(),
			"securitySchemes": M{
				"bearerAuth":   M{"type": "http", "scheme": "bearer", "bearerFormat": "hxg_…", "description": "API key as a bearer token"},
				"apiKeyHeader": M{"type": "apiKey", "in": "header", "name": "X-API-Key"},
				"apiKeyQuery":  M{"type": "apiKey", "in": "query", "name": "api_key", "description": "For WebSocket clients"},
			},
			"responses": M{
				"Unauthorized": M{"description": "Missing, invalid or revoked API key", "content": errBody},
				"Forbidden":    M{"description": "The key lacks the required scope", "content": errBody},
				"RateLimited": M{"description": "Rate limit exceeded; see Retry-After", "content": errBody,
					"headers": M{"Retry-After": M{"schema": M{"type": "integer"}, "description": "Seconds"},
						"X-RateLimit-Limit": rateHeaders["X-RateLimit-Limit"], "X-RateLimit-Remaining": rateHeaders["X-RateLimit-Remaining"],
						"X-RateLimit-Reset": rateHeaders["X-RateLimit-Reset"]}},
			},
			"headers": rateHeaders,
		},
	}
}

var (
	specOnce sync.Once
	specJSON []byte
)

// JSON returns the indented document.
func JSON() []byte {
	specOnce.Do(func() {
		b, err := json.MarshalIndent(Spec(), "", "  ")
		if err != nil {
			panic(err) // static data: a failure is a programming error caught by tests
		}
		specJSON = append(b, '\n')
	})
	return specJSON
}

// SpecHandler serves /openapi.json.
func SpecHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(JSON())
}

// swaggerUI loads Swagger UI 5 (OpenAPI 3.1 support) from jsDelivr, pinned.
const swaggerUI = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HexaGo API</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'%3E%3Cpolygon points='50,4 90,27 90,73 50,96 10,73 10,27' fill='%23FFC400'/%3E%3C/svg%3E">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
<style>body{margin:0;background:#fafafa}.topbar{display:none}.hx{font:600 14px system-ui,sans-serif;padding:12px 20px;background:#0b0f19;color:#ffc400}</style>
</head>
<body>
<div class="hx">⬢ HexaGo Public API · get a key: POST /v1/developer/keys · then Authorize with it</div>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
<script>
window.ui = SwaggerUIBundle({url: "/openapi.json", dom_id: "#swagger-ui", deepLinking: true,
  persistAuthorization: true, displayRequestDuration: true, tryItOutEnabled: false});
</script>
</body>
</html>
`

// SwaggerHandler serves /docs.
func SwaggerHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerUI))
}
