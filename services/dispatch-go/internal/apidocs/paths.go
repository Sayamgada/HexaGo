package apidocs

const (
	tagSystem    = "System"
	tagRides     = "Rides"
	tagStream    = "Stream"
	tagBooking   = "Booking"
	tagPricing   = "Pricing"
	tagSafety    = "Safety"
	tagLedger    = "Ledger"
	tagAgents    = "Agents"
	tagSim       = "Simulator"
	tagDeveloper = "Developer"
)

const (
	sRidesR  = "rides:read"
	sRidesW  = "rides:write"
	sPricing = "pricing:read"
	sStream  = "stream:read"
	sAgents  = "agents:invoke"
	sLedger  = "ledger:read"
)

func paths() M {
	limit := query("limit", "integer", "Maximum items")
	since := query("since", "string", "RFC 3339 lower bound")
	return M{
		// ---- system ----
		"/healthz": M{"get": op(tagSystem, "getHealth", "Liveness and component status", "",
			query("redis", "string", "fresh = run a live Redis check first"), ok(200, "Healthy", ref("Health")))},
		"/metrics": M{"get": op(tagSystem, "getPrometheusMetrics", "Prometheus metrics", "",
			content(200, "Prometheus text exposition", "text/plain"))},
		"/v1/ping": M{"get": op(tagSystem, "ping", "Gateway ping", "", ok(200, "Pong", ref("Pong")))},
		"/openapi.json": M{"get": op(tagSystem, "getOpenAPI", "This OpenAPI 3.1 document", "",
			ok(200, "OpenAPI document", anyObj("")))},
		"/docs": M{"get": op(tagSystem, "getDocs", "Swagger UI", "", content(200, "HTML page", "text/html"))},

		// ---- rides ----
		"/v1/rides": M{"post": op(tagRides, "createRide", "Request a ride (matched synchronously when a driver is free)", sRidesW,
			body(ref("CreateRide"), true),
			ok(201, "Matched: ride and match details", ref("RideView")),
			ok(202, "No driver yet: stored as an open request", ref("RideView")), fails(400, 409))},
		"/v1/rides/{id}": M{"get": op(tagRides, "getRide", "Get a ride", sRidesR, path("id", "Ride UUID"),
			ok(200, "The ride", ref("RideView")), fails(404))},
		"/v1/rides/{id}/cancel": M{"post": op(tagRides, "cancelRide", "Cancel a ride", sRidesW, path("id", "Ride UUID"),
			body(obj(nil, M{"reason": str("")}), false), ok(200, "Cancelled", ref("RideView")), fails(400, 404, 409))},
		"/v1/drivers/{id}/status": M{"post": op(tagRides, "updateDriverStatus", "Driver telemetry / status update (creates the driver on first call)", sRidesW,
			path("id", "Driver id"), body(ref("DriverStatusInput"), true), ok(200, "Driver state", ref("DriverView")), fails(400, 404))},
		"/v1/drivers": M{"get": op(tagRides, "listDrivers", "Simulated drivers for the Driver module picker (status, tier, AV, suspension)", sRidesR,
			query("tier", "string", "Only this tier"), query("q", "string", "Driver id substring"),
			ok(200, "Drivers", anyObj("{drivers: [...], count, simulated}")))},
		"/v1/drivers/{id}": M{"get": op(tagRides, "getDriver", "One driver's live state, current ride (ETA, progress), AV panel and Safety suspension", sRidesR,
			path("id", "Driver id"), ok(200, "Driver", anyObj("Driver detail")), fails(404))},
		"/v1/metrics/dispatch": M{"get": op(tagRides, "getDispatchMetrics", "Dispatch latency percentiles and fleet counts", sRidesR,
			ok(200, "Metrics", ref("DispatchMetrics")))},
		"/v1/eta": M{"get": op(tagRides, "getETA", "Road distance and duration between two points", sRidesR,
			param("query", "from_lat", "number", "", true), param("query", "from_lng", "number", "", true),
			param("query", "to_lat", "number", "", true), param("query", "to_lng", "number", "", true),
			ok(200, "Estimate", ref("ETA")), fails(400))},
		"/v1/supply": M{"get": op(tagRides, "getSupply", "Idle drivers and open requests around a point (H3)", sRidesR,
			param("query", "lat", "number", "", true), param("query", "lng", "number", "", true),
			ok(200, "Cell load", ref("Supply")), fails(400))},
		"/v1/supply/snapshot": M{"get": op(tagRides, "getSupplySnapshot", "Every vehicle's position and the recent ride requests (pricing models bin these)", sRidesR,
			ok(200, "Snapshot", ref("SupplySnapshot")))},
		"/v1/metrics/counters": M{"get": op(tagRides, "getDailyCounters", "Matches, settled rides and agent decisions today (IST)", sRidesR,
			ok(200, "Counters", ref("Counters")), fails(503))},
		"/v1/scheduled": M{
			"get": op(tagRides, "listScheduled", "Scheduled legs of confirmed booking plans", sRidesR,
				query("rider_id", "string", ""), query("plan_id", "string", ""), ok(200, "Legs", ref("ScheduledList"))),
			"post": op(tagRides, "schedulePlan", "Schedule the legs of a plan (the Booking Intent agent calls this on confirm)", sRidesW,
				body(ref("SchedulePlan"), true), ok(201, "Scheduled legs", ref("ScheduledList")), fails(400, 409)),
		},

		// ---- stream ----
		"/v1/stream": M{"get": op(tagStream, "openStream", "Live WebSocket stream (drivers, ride:<id>, ops)", sStream,
			desc("Upgrade to a WebSocket, then send `{\"action\":\"subscribe\",\"topics\":[\"ride:<id>\"]}` (schema StreamControl). "+
				"Frames are `{\"topic\",\"data\"}` (schema StreamFrame), batched once per second: `drivers` (positions), "+
				"`ride:<id>` (status, driver position, ETA), `ops` (agent events and metrics). "+
				"Browsers cannot set headers on WebSockets, so pass the key as `?api_key=hxg_…`."),
			query("api_key", "string", "API key (WebSocket clients)"),
			ok(101, "Switching protocols", nil), fails(400))},
		"/v1/ws": M{"get": op(tagStream, "openEchoSocket", "Echo WebSocket (connectivity check)", sStream,
			ok(101, "Switching protocols", nil))},

		// ---- simulator ----
		"/v1/sim/config": M{
			"get": op(tagSim, "getSimConfig", "Fleet simulator status (DEMO_MODE)", sRidesR, ok(200, "Status", ref("SimConfig")), fails(503)),
			"post": op(tagSim, "configureSim", "Resize the fleet, change demand or add a demand spike", sAgents,
				body(ref("SimConfigInput"), true), ok(200, "New status", ref("SimConfig")), fails(400, 503)),
		},
		"/v1/sim/scenario/{name}": M{"post": op(tagSim, "runScenario", "Trigger a scenario", sAgents,
			path("name", "rush_hour | rain_storm | concert_exit | safety_incident"), ok(200, "Status", ref("SimConfig")), fails(400, 404, 503))},

		// ---- safety: dispatch-go ----
		"/v1/safety/signals": M{"get": op(tagSafety, "listSafetySignals", "Recent detector signals and thresholds", sRidesR,
			ok(200, "Signals", anyObj("")))},
		"/v1/safety/drivers/{id}": M{"get": op(tagSafety, "getDriverSuspension", "Driver suspension state", sRidesR,
			path("id", "Driver id"), ok(200, "State", ref("Suspension")))},
		"/v1/safety/drivers/{id}/suspend": M{"post": op(tagSafety, "suspendDriver", "Suspend a driver (Safety Sentinel escalation)", sAgents,
			path("id", "Driver id"), body(obj([]string{"incident_id"}, M{"incident_id": str(""), "reason": str(""), "actor": str("")}), true),
			ok(200, "Suspended", anyObj("")), fails(400))},
		"/v1/safety/drivers/{id}/unsuspend": M{"post": op(tagSafety, "unsuspendDriver", "Release a driver (Ops Supervisor confirmation required)", sAgents,
			path("id", "Driver id"), body(obj([]string{"incident_id", "confirmed_by"}, M{"incident_id": str(""), "confirmed_by": enum("", "ops_supervisor")}), true),
			ok(200, "Released", anyObj("")), fails(400, 403, 404))},
		"/v1/safety/track": M{"post": op(tagSafety, "createTrackLink", "Create a live-location share link", sAgents,
			body(obj([]string{"driver_id"}, M{"driver_id": str(""), "ride_id": str(""), "incident_id": str("")}), true),
			ok(200, "Link", obj([]string{"token", "url"}, M{"token": str(""), "url": str("")})), fails(400))},
		"/track/{token}": M{"get": op(tagSafety, "getTrackedLocation", "Live location behind a share link", "",
			path("token", "Share token"), ok(200, "Location", anyObj("")), fails(404))},
		"/v1/safety/vehicle-path/{driverID}": M{"get": op(tagSafety, "getVehiclePath", "Recent telemetry of a vehicle, oldest first", sRidesR,
			path("driverID", "Driver id"), limit, since, ok(200, "Path", anyObj("")), fails(400))},
		"/v1/safety/publish": M{"post": op(tagSafety, "publishSafetyFrame", "Publish a safety_call / safety_update frame on the stream", sAgents,
			body(obj([]string{"topic", "data"}, M{"topic": str("ops | ride:<id>"), "data": anyObj("")}), true),
			ok(202, "Published", anyObj("")), fails(400, 503))},
		"/v1/safety/emergency/dispatch": M{"post": op(tagSafety, "dispatchEmergency", "Simulated emergency services dispatch", sAgents,
			body(anyObj(""), true), ok(200, "Simulated dispatch", anyObj("")), fails(400))},
		"/v1/safety/emergency/notify-contact": M{"post": op(tagSafety, "notifyEmergencyContact", "Simulated emergency-contact SMS", sAgents,
			body(anyObj(""), true), ok(200, "Simulated SMS", anyObj("")), fails(400))},
		"/v1/safety/emergency/log": M{"get": op(tagSafety, "getEmergencyLog", "Simulated emergency records", sRidesR,
			ok(200, "Records", obj([]string{"records"}, M{"records": arr(anyObj(""))})))},

		// ---- safety: agents-py ----
		"/v1/safety/signal": M{"post": op(tagSafety, "submitSafetySignal", "Submit a detector signal to the Safety Sentinel", sAgents, proxied(),
			body(ref("SafetySignal"), true), ok(202, "Decision", ref("SignalDecision")), fails(422))},
		"/v1/safety/incidents": M{"get": op(tagSafety, "listIncidents", "Safety incidents, newest first", sRidesR, proxied(),
			query("status", "string", ""), limit, ok(200, "Incidents", ref("IncidentList")))},
		"/v1/safety/incidents/{incident_id}": M{"get": op(tagSafety, "getIncident", "An incident with its timeline", sRidesR, proxied(),
			path("incident_id", ""), ok(200, "Incident", ref("IncidentDetail")), fails(404))},
		"/v1/safety/incidents/{incident_id}/resolve": M{"post": op(tagSafety, "resolveIncident", "Resolve an incident (Ops Supervisor confirms)", sAgents, proxied(),
			path("incident_id", ""), body(obj(nil, M{"requested_by": str(""), "note": str("")}), true),
			ok(200, "Resolved", ref("Incident")), fails(404, 409, 502))},
		"/v1/safety/checkins": M{"get": op(tagSafety, "listCheckins", "Rider safety check-ins", sRidesR, proxied(),
			query("status", "string", "pending (default) | all | answered | expired"), ok(200, "Check-ins", ref("CheckinList")))},
		"/v1/safety/checkins/{checkin_id}": M{"get": op(tagSafety, "getCheckin", "A check-in", sRidesR, proxied(),
			path("checkin_id", ""), ok(200, "Check-in", ref("Checkin")), fails(404))},
		"/v1/safety/checkins/{checkin_id}/reply": M{"post": op(tagSafety, "replyCheckin", "The rider answers a safety call", sRidesW, proxied(),
			path("checkin_id", ""), body(obj(nil, M{"transcript": str("What the rider said, e.g. \"I'm fine\"")}), true),
			ok(200, "Outcome", anyObj("")), fails(404, 409))},

		// ---- ledger ----
		"/v1/wallets/{owner}": M{"get": op(tagLedger, "getWallet", "Balances and recent payouts of a rider, driver or operator", sLedger,
			path("owner", "Rider, driver or operator id"), ok(200, "Wallet", ref("Wallet")), fails(400))},
		"/v1/wallets/{owner}/topup": M{"post": op(tagLedger, "topupWallet", "Top up a rider wallet (DEMO_MODE only)", sRidesW,
			path("owner", "Rider id"), body(ref("Topup"), true), ok(201, "Entry and wallet", ref("TopupResult")), fails(400, 403))},
		"/v1/ledger/entries": M{"get": op(tagLedger, "listEntries", "Journal entries (or one ride's entries with ride_id)", sLedger,
			query("ride_id", "string", "Return this ride's entries, fare breakdown and payout"), query("kind", "string", ""), since, limit,
			ok(200, "Entries (EntryList), or RideLedger when ride_id is set", M{"anyOf": []M{ref("RideLedger"), ref("EntryList")}}), fails(400))},
		"/v1/ledger/trial-balance": M{"get": op(tagLedger, "getTrialBalance", "Trial balance (always sums to zero)", sLedger,
			ok(200, "Trial balance", ref("TrialBalance")))},
		"/v1/ledger/summary": M{"get": op(tagLedger, "getLedgerSummary", "Money summary for today", sLedger,
			ok(200, "Summary", ref("LedgerSummary")))},
		"/v1/ledger/payouts": M{"get": op(tagLedger, "listPayouts", "Driver / operator payouts", sLedger,
			query("status", "string", "pending | held_for_review | sent"), query("driver_id", "string", ""),
			query("incident_id", "string", ""), query("ride_id", "string", ""), since, limit,
			ok(200, "Payouts", ref("PayoutList")), fails(400))},
		"/v1/ledger/refunds": M{"post": op(tagLedger, "createRefund", "Refund a ride (a reversal entry)", sRidesW,
			body(ref("Refund"), true), ok(201, "Refund entry", ref("Entry")), fails(400, 404, 409))},

		// ---- agents ----
		"/v1/agents": M{"get": op(tagAgents, "listAgents", "The 11-agent roster with status and recent decisions", sAgents, proxied(),
			ok(200, "Roster", ref("AgentList")))},
		"/v1/agents/events": M{"get": op(tagAgents, "listAgentEvents", "Agent audit events, newest first", sAgents, proxied(),
			query("limit", "integer", "1..500, default 50"), query("agent", "string", "Agent id"),
			query("per_agent", "integer", "1..100: keep at most this many events per agent (last 24 h); ignored with agent"), ok(200, "Events", ref("AgentEventList")), fails(422))},
		"/v1/agents/llm/budget": M{"get": op(tagAgents, "getLLMBudget", "LLM calls and tokens used today vs the guardrails", sAgents, proxied(),
			ok(200, "Budget", ref("LLMBudget")))},
		"/v1/agents/ping": M{"get": op(tagAgents, "pingAgents", "agents-py ping", sAgents, proxied(), ok(200, "Pong", ref("Pong")))},
		"/v1/agents/parse-intent": M{"post": op(tagAgents, "parseIntent", "Legacy single-shot intent parser", sAgents, proxied(),
			body(ref("ParseIntentInput"), true), ok(200, "Extracted fields", ref("ParseIntent")), fails(422))},
		"/v1/agents/settlement/findings": M{"get": op(tagAgents, "getSettlementFindings", "Settlement agent audit findings", sAgents, proxied(),
			ok(200, "Findings", ref("SettlementFindings")))},
		"/v1/agents/settlement/scan": M{"post": op(tagAgents, "runSettlementScan", "Run a settlement audit now", sAgents, proxied(),
			ok(200, "Findings", ref("SettlementFindings")))},

		// ---- booking (agents-py) ----
		"/v1/agents/booking/personas": M{"get": op(tagBooking, "listPersonas", "Demo rider personas and saved places", sAgents, proxied(),
			ok(200, "Personas", ref("Personas")))},
		"/v1/agents/booking/parse": M{"post": op(tagBooking, "parseBooking", "Turn one sentence into a priced, timed ride plan", sAgents, proxied(),
			query("force_fallback", "boolean", "DEMO_MODE only: skip the LLM"), body(ref("BookingParse"), true),
			ok(200, "A plan, or a clarifying question", ref("BookingPlan")), fails(403, 404, 422))},
		"/v1/agents/booking/confirm": M{"post": op(tagBooking, "confirmBooking", "Confirm a plan: its legs are scheduled and dispatched", sAgents, proxied(),
			body(ref("BookingConfirm"), true), ok(200, "Scheduled rides", anyObj("")), fails(404, 422, 502))},
		"/v1/agents/booking/voice": M{"post": op(tagBooking, "transcribeBooking", "Transcribe a spoken request (providers with a transcription API)", sAgents, proxied(),
			rawBody("audio/webm", "Raw audio body, audio/* content type, up to 10 MB"),
			ok(200, "Transcript", obj([]string{"text"}, M{"text": str("")})), fails(400, 413, 415, 502))},

		// ---- pricing (agents-py) ----
		"/v1/pricing/tiers": M{"get": op(tagPricing, "getTiers", "Rate card", sPricing, proxied(), ok(200, "Tiers", ref("Tiers")))},
		"/v1/pricing/surge-map": M{"get": op(tagPricing, "getSurgeMap", "Surge multipliers per H3 cell", sPricing, proxied(),
			ok(200, "Surge map", ref("SurgeMap")))},
		"/v1/pricing/quote": M{"post": op(tagPricing, "getQuote", "Fare quote with surge", sPricing, proxied(),
			body(ref("QuoteInput"), true), ok(200, "Quote", ref("Quote")), fails(422))},

		// ---- developer platform ----
		"/v1/developer/scopes": M{"get": op(tagDeveloper, "listScopes", "Scopes, webhook events and the signature scheme", "",
			ok(200, "Scopes", ref("Scopes")))},
		"/v1/developer/keys": M{"post": op(tagDeveloper, "createKey", "Create an API key (self-serve demo; the key is shown once)", "",
			desc("Limited to 5 keys per minute per client IP. Keys are stored as SHA-256 hashes."),
			body(ref("CreateKey"), false), ok(201, "The new key", ref("CreatedKey")), fails(400),
			func(o M) { o["responses"].(M)["429"] = M{"$ref": "#/components/responses/RateLimited"} })},
		"/v1/developer/keys/me": M{
			"get": op(tagDeveloper, "getMyKey", "The calling key", "", keyed(), ok(200, "Key", ref("Key"))),
			"delete": op(tagDeveloper, "revokeMyKey", "Revoke the calling key (and its webhooks)", "", keyed(),
				ok(200, "Revoked", obj([]string{"revoked"}, M{"revoked": boolean(""), "id": str("")}))),
		},
		"/v1/developer/webhooks": M{
			"post": op(tagDeveloper, "createWebhook", "Subscribe a URL to events (the signing secret is shown once)", "", keyed(),
				desc("Deliveries are POSTs with `X-HexaGo-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + \".\" + body))` "+
					"and `X-HexaGo-Timestamp`; non-2xx answers are retried 3 times with exponential backoff (1 s, 2 s, 4 s). "+
					"Private, loopback and link-local targets are refused."),
				body(ref("CreateWebhook"), true), ok(201, "Webhook", ref("CreatedWebhook")), fails(400)),
			"get": op(tagDeveloper, "listWebhooks", "The calling key's webhooks", "", keyed(), ok(200, "Webhooks", ref("WebhookList"))),
		},
		"/v1/developer/webhooks/{id}": M{"delete": op(tagDeveloper, "deleteWebhook", "Delete a webhook", "", keyed(),
			path("id", ""), ok(200, "Deleted", anyObj("")), fails(404))},
		"/v1/developer/webhooks/{id}/test": M{"post": op(tagDeveloper, "testWebhook", "Queue a ping delivery", "", keyed(),
			path("id", ""), ok(202, "Queued", anyObj("")), fails(404))},
		"/v1/developer/webhooks/{id}/deliveries": M{"get": op(tagDeveloper, "listDeliveries", "Recent deliveries to a webhook", "", keyed(),
			path("id", ""), ok(200, "Deliveries", ref("DeliveryList")), fails(404))},
	}
}

// keyed marks a developer operation that needs a key (any scope).
func keyed() opt {
	return func(o M) {
		o["security"] = []M{{"bearerAuth": []string{}}, {"apiKeyHeader": []string{}}}
		o["description"] = "Needs an API key (any scope)."
		o["responses"].(M)["401"] = M{"$ref": "#/components/responses/Unauthorized"}
	}
}
