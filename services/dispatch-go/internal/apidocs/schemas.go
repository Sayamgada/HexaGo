package apidocs

func schemas() M {
	latLng := obj([]string{"lat", "lng"}, M{"lat": num("Latitude"), "lng": num("Longitude")})
	tier := enum("Vehicle tier", "auto", "mini", "sedan", "premium", "xl", "av", "pool")
	return M{
		"Error": obj(nil, M{
			"error":  str("What went wrong (dispatch-go)"),
			"detail": M{"description": "What went wrong (agents-py / FastAPI validation)"},
		}),
		"LatLng": latLng,
		"Tier":   tier,

		// ---- system ----
		"Health": obj([]string{"status", "service"}, M{
			"status": enum("", "ok"), "service": str(""), "demo_mode": boolean(""),
			"components":  anyObj("Store, fleet mirror, geo backend, stream broker, simulator, API-key store, rate limiter"),
			"redis":       anyObj("Last Redis check (?redis=fresh runs a new one)"),
			"city_center": ref("LatLng"),
		}),
		"Pong": obj([]string{"message", "service"}, M{"message": str(""), "service": str("")}),

		// ---- rides ----
		"RideLeg": obj([]string{"seq", "kind", "from", "to"}, M{
			"seq": integer(""), "kind": enum("", "pickup", "trip"), "from": ref("LatLng"), "to": ref("LatLng"),
			"distance_km": num(""), "eta_seconds": num(""),
		}),
		"Ride": obj([]string{"id", "rider_id", "tier", "status", "pickup", "dropoff", "created_at"}, M{
			"id": str("Ride UUID"), "rider_id": str(""), "driver_id": str("Set once matched"), "tier": str(""),
			"status": enum("requested → matched → in_progress → completed, or cancelled", "requested", "matched", "in_progress", "completed", "cancelled"),
			"pickup": ref("LatLng"), "dropoff": ref("LatLng"), "pickup_h3": str("H3 cell of the pickup"),
			"eta_seconds": num("Driver → pickup"), "distance_km": num("Pickup → drop-off road estimate"),
			"match_score": num(""), "cancel_reason": str(""), "legs": orNull(arr(ref("RideLeg"))),
			"created_at": dt(""), "updated_at": dt(""),
		}),
		"Driver": obj([]string{"id", "lat", "lng", "status", "tier"}, M{
			"id": str(""), "lat": num(""), "lng": num(""), "heading": num("Degrees from north"), "speed_kmh": num(""),
			"status": enum("", "offline", "idle", "en_route", "on_trip"), "tier": str(""),
			"battery_pct": num("AVs only"), "rating": num("1..5"), "idle_since": dt(""), "updated_at": dt(""),
			"h3_cell": str(""),
		}),
		"Match": obj([]string{"driver"}, M{
			"driver": ref("Driver"), "ring": integer("H3 ring the driver was found in"), "score": anyObj("Score breakdown"),
			"alternatives": orNull(arr(anyObj("Runner-up driver"))), "candidates_considered": integer(""),
			"rings_searched": integer(""), "search_radius_km": num(""), "tier": str("Tier of the assigned car"),
			"requested_tier": str(""), "upgraded": boolean("Free upgrade applied"), "latency_ms": num(""),
		}),
		"RideView": obj([]string{"ride"}, M{"ride": ref("Ride"), "match": ref("Match")}),
		"CreateRide": obj([]string{"rider_id", "pickup", "dropoff"}, M{
			"rider_id": str(""), "pickup": ref("LatLng"), "dropoff": ref("LatLng"), "tier": withDesc(tier, "Default mini"),
		}),
		"DriverStatusInput": obj(nil, M{
			"status": enum("", "offline", "idle", "en_route", "on_trip"), "lat": num(""), "lng": num(""), "heading": num(""),
			"speed_kmh": num(""), "tier": str(""), "battery_pct": num(""), "rating": num(""),
		}),
		"DriverView": obj([]string{"driver"}, M{"driver": ref("Driver")}),
		"DispatchMetrics": obj([]string{"p50_ms", "p95_ms", "p99_ms", "matches_total"}, M{
			"p50_ms": num("Match latency"), "p95_ms": num(""), "p99_ms": num(""), "matches_total": integer(""),
			"active_drivers": integer(""), "idle_drivers": integer(""), "open_requests": integer(""), "rides_in_progress": integer(""),
		}),
		"ETA": obj([]string{"distance_km", "duration_min", "source"}, M{
			"distance_km": num(""), "duration_min": num(""), "duration_s": num(""), "source": str(""),
		}),
		"Supply": obj([]string{"cell", "supply", "demand"}, M{
			"cell": str("H3 cell"), "supply": integer("Idle drivers nearby"), "demand": integer("Open requests nearby"),
			"rings": integer(""), "cells": integer(""),
		}),
		"ScheduledLeg": obj([]string{"id", "plan_id", "rider_id", "seq", "pickup", "dropoff", "pickup_at", "status"}, M{
			"id": str(""), "plan_id": str(""), "rider_id": str(""), "seq": integer(""), "passenger": str(""),
			"passenger_name": str(""), "tier": str(""), "dispatch_tier": str(""), "pickup": ref("LatLng"), "dropoff": ref("LatLng"),
			"pickup_name": str(""), "dropoff_name": str(""), "pickup_at": dt(""), "arrive_by": dt(""), "dispatch_at": dt(""),
			"fare": num("INR"), "duration_min": num(""), "status": str("scheduled | dispatched | completed | failed | cancelled"),
			"ride_id": str(""), "ride_status": str(""), "driver_id": str(""), "note": str(""),
		}),
		"ScheduledList": obj([]string{"legs"}, M{"count": integer(""), "demo_mode": boolean(""), "legs": arr(ref("ScheduledLeg"))}),
		"SupplySnapshot": obj([]string{"as_of", "drivers", "requests"}, M{
			"as_of": dt(""), "window_s": integer("Request window in seconds"),
			"drivers": orNull(arr(obj([]string{"lat", "lng", "status"}, M{
				"lat": num(""), "lng": num(""), "status": str(""),
			}))),
			"requests": orNull(arr(obj([]string{"lat", "lng"}, M{"lat": num(""), "lng": num(""), "at": dt(""), "matched": boolean("")}))),
			"counts": mapOf(integer("")),
		}),
		"Counters": obj([]string{"matches_today", "rides_settled_today", "decisions_today"}, M{
			"matches_today": integer("Since midnight IST"), "rides_settled_today": integer(""), "decisions_today": integer(""),
			"since": dt(""), "source": str("postgres | memory"), "as_of": dt(""),
		}),
		"SurgeMap": anyObj("H3 resolution-8 cells with surge multiplier and boundary (Surge Equilibrium agent, refreshed every 30 s)"),
		"SchedulePlan": obj([]string{"plan_id", "rider_id", "legs"}, M{
			"plan_id": str(""), "rider_id": str(""),
			"legs": arr(obj([]string{"seq", "pickup", "dropoff", "pickup_at"}, M{
				"seq": integer(""), "passenger": str(""), "passenger_name": str(""), "tier": str(""),
				"pickup": ref("LatLng"), "dropoff": ref("LatLng"), "pickup_name": str(""), "dropoff_name": str(""),
				"pickup_at": dt(""), "arrive_by": orNull(dt("")), "fare": num(""), "duration_min": num(""),
			})),
		}),

		// ---- stream ----
		"StreamFrame": obj([]string{"topic", "data"}, M{
			"topic": str("drivers | ops | ride:<id>"),
			"data":  M{"description": "drivers: Driver[]; ride:<id>: {ride_id, status, driver, eta_sec}; ops: agent_event | metrics | safety_call | safety_update"},
		}),
		"StreamControl": obj([]string{"action", "topics"}, M{
			"action": enum("", "subscribe", "unsubscribe"), "topics": arr(str("drivers | ops | ride:<id>")),
		}),

		// ---- simulator ----
		"SimConfig": obj([]string{"running", "drivers"}, M{
			"running": boolean(""), "drivers": integer(""), "live_drivers": integer(""), "active_rides": integer(""),
			"demand_per_min": num(""), "current_demand_per_min": num(""), "auto_demand": boolean(""),
			"scenarios": arr(anyObj("")), "spikes": arr(anyObj("")), "router": str(""),
		}),
		"SimConfigInput": obj(nil, M{"drivers": integer("0..2000"), "demand_per_min": num(""), "spike_lat": num(""), "spike_lng": num("")}),

		// ---- safety ----
		"Incident": obj([]string{"id", "ride_id", "driver_id", "status", "severity"}, M{
			"id": str(""), "ride_id": str(""), "driver_id": str(""), "rider_id": orNull(str("")), "tier": orNull(str("")),
			"severity": num("0..1"), "status": str("check_in | escalated | monitoring | resolved"),
			"decision": str("monitor | check_in | escalate"), "reason": str(""), "triggers": arr(anyObj("")),
			"location": orNull(ref("LatLng")), "created_at": dt(""), "updated_at": dt(""), "resolved_at": orNull(dt("")),
			"pending_checkin_id": orNull(str("")), "flags": anyObj(""),
		}),
		"IncidentList":   obj([]string{"incidents"}, M{"incidents": arr(ref("Incident")), "count": integer("")}),
		"IncidentDetail": M{"allOf": []M{ref("Incident"), obj(nil, M{"timeline": arr(anyObj("Timeline step")), "risk": orNull(str(""))})}},
		"Checkin": obj([]string{"id", "status"}, M{
			"id": str(""), "incident_id": str(""), "ride_id": str(""), "status": str("pending | answered | expired"),
			"prompt": str(""), "created_at": M{}, "expires_at": M{},
		}),
		"CheckinList": obj([]string{"checkins"}, M{"checkins": arr(ref("Checkin")), "count": integer("")}),
		"SafetySignal": obj([]string{"ride_id", "driver_id", "severity"}, M{
			"signal_id": str(""), "ride_id": str(""), "driver_id": str(""), "rider_id": str(""), "tier": str(""),
			"severity": num("0..1"), "triggers": arr(anyObj("")), "location": ref("LatLng"),
		}),
		"SignalDecision": obj([]string{"action"}, M{
			"action": enum("", "monitor", "check_in", "escalate"), "incident_id": orNull(str("")), "checkin_id": str(""),
			"severity": num(""), "reason": str(""), "latency_ms": num(""), "suppressed": boolean(""),
		}),
		"Suspension": obj(nil, M{"driver_id": str(""), "suspended": boolean(""), "suspension": anyObj("")}),

		// ---- ledger ----
		"Posting": obj([]string{"account", "amount_paise"}, M{
			"account": str("type:owner"), "account_type": str(""), "owner": str(""),
			"amount_paise": integer("Signed; every entry sums to zero"), "memo": str(""),
		}),
		"Entry": obj([]string{"id", "kind", "postings", "created_at"}, M{
			"id": str(""), "seq": integer(""), "kind": str("ride | payout | refund | topup"), "idempotency_key": str(""),
			"ride_id": str(""), "reverses_id": str(""), "description": str(""), "meta": anyObj(""),
			"postings": arr(ref("Posting")), "created_at": dt(""),
		}),
		"Payout": obj([]string{"id", "ride_id", "amount_paise", "status"}, M{
			"id": str(""), "ride_id": str(""), "account": str(""), "account_type": str(""), "owner": str(""),
			"amount_paise": integer(""), "status": enum("", "pending", "held_for_review", "sent"),
			"incident_id": str(""), "driver_id": str(""), "entry_id": str(""), "created_at": dt(""),
			"held_at": dt(""), "released_at": dt(""), "sent_at": dt(""),
		}),
		"Wallet": obj([]string{"owner", "balance_paise"}, M{
			"owner": str(""), "balance_paise": integer(""), "accounts": orNull(arr(anyObj("Account balance"))),
			"payouts": orNull(arr(ref("Payout"))), "paid_out_paise": integer(""), "held_paise": integer(""),
		}),
		"Topup":       obj([]string{"amount_paise"}, M{"amount_paise": integer("> 0"), "idempotency_key": str("")}),
		"TopupResult": obj([]string{"entry", "wallet"}, M{"entry": ref("Entry"), "wallet": ref("Wallet")}),
		"EntryList":   obj([]string{"entries"}, M{"entries": arr(ref("Entry"))}),
		"RideLedger": obj([]string{"ride_id", "entries"}, M{
			"ride_id": str(""), "entries": arr(ref("Entry")), "breakdown": orNull(anyObj("Fare split")), "payout": ref("Payout"),
		}),
		"TrialBalance": obj([]string{"total_paise", "balanced", "types"}, M{
			"total_paise": integer("Always 0"), "balanced": boolean(""), "types": arr(anyObj("Per account type")),
			"entries": integer(""), "postings": integer(""), "as_of": dt(""),
		}),
		"LedgerSummary": obj([]string{"balanced", "payouts_today"}, M{
			"trial_balance_paise": integer(""), "balanced": boolean(""), "payouts_today": anyObj(""),
			"platform_revenue_paise": integer(""), "rides_settled": integer(""), "entries": integer(""),
			"store": str(""), "as_of": dt(""),
		}),
		"PayoutList": obj([]string{"payouts"}, M{"payouts": arr(ref("Payout")), "store": str(""), "rates": anyObj("Split rates in basis points")}),
		"Refund": obj([]string{"ride_id", "amount_paise", "reason"}, M{
			"ride_id": str(""), "amount_paise": integer(""), "reason": str(""), "idempotency_key": str(""),
		}),

		// ---- agents ----
		"Agent": obj([]string{"id", "name", "status"}, M{
			"id": str(""), "name": str(""), "responsibility": str(""), "description": str(""), "fallback_mode": str(""),
			"status": str("active | idle | degraded"), "runs_in": orNull(str("agents-py | dispatch-go")), "decisions_last_hour": integer(""),
			"fallback_rate": orNull(num("")), "last_decision": orNull(anyObj("")),
		}),
		"AgentList": obj([]string{"agents", "total"}, M{
			"agents": arr(ref("Agent")), "active": integer(""), "total": integer(""), "events_backend": str(""),
		}),
		"AgentEvent": obj([]string{"agent", "decision"}, M{
			"type": str(""), "agent": str(""), "decision": str(""), "reasoning": str(""), "confidence": num(""),
			"latency_ms": num(""), "used_fallback": boolean(""), "ts": str(""), "kind": str(""), "ride_id": str(""),
		}),
		"AgentEventList": obj([]string{"events"}, M{"events": arr(ref("AgentEvent")), "count": integer(""), "backend": str("")}),
		"LLMBudget": obj([]string{"calls_today", "tokens_today", "limits", "enabled"}, M{
			"day": str(""), "timezone": str(""), "enabled": boolean(""), "model": str(""),
			"fallback_models": arr(str("")), "calls_today": integer(""), "tokens_today": integer(""),
			"cache_hits": integer(""), "limits": anyObj(""), "store": str(""),
		}),
		"Personas": obj([]string{"personas"}, M{"personas": arr(anyObj("Demo rider with saved places")), "demo_mode": boolean("")}),
		"BookingParse": obj([]string{"text"}, M{
			"rider_id": str("Demo persona, e.g. rider_priya"), "text": str("One sentence, 1..1000 chars"),
			"reply_to": orNull(str("Question id when answering a clarifying question")),
		}),
		"BookingPlan": obj([]string{"type"}, M{
			"type":    enum("plan: a priced, timed multi-leg plan; question: the agent needs a clarification", "plan", "question"),
			"plan_id": str(""), "rider_id": str(""), "legs": arr(anyObj("Leg with origin, destination, times and price")),
			"total_fare": num(""), "currency": str(""), "warnings": arr(str("")), "answered_by": anyObj("Model or rule-based fallback"),
			"question": str(""), "question_id": str(""),
		}),
		"BookingConfirm":     obj([]string{"plan_id"}, M{"plan_id": str("")}),
		"ParseIntentInput":   obj([]string{"query"}, M{"query": str(""), "agent": str("")}),
		"ParseIntent":        obj([]string{"agent", "extracted"}, M{"agent": str(""), "extracted": anyObj(""), "fallback_mode": boolean("")}),
		"SettlementFindings": obj([]string{"findings"}, M{"last_run": orNull(anyObj("")), "runs": integer(""), "findings": arr(anyObj("Flagged entry or payout"))}),

		// ---- pricing ----
		"Tiers": obj([]string{"currency", "tiers"}, M{"currency": str(""), "surge_cap": num(""), "tiers": mapOf(obj(nil, M{
			"label": str(""), "base": num(""), "per_km": num(""), "per_min": num(""), "minimum": num(""),
		}))}),
		"ForecastCell": obj([]string{"h3", "boundary", "demand_score", "expected_earnings_per_hr", "explanation"}, M{
			"h3": str("H3 res-8 cell"), "center": ref("LatLng"), "boundary": arr(arr(num("lat, lng"))),
			"demand_score": num("0..1, absolute (12 rides/h = 0.5)"), "expected_requests_per_hr": num(""),
			"expected_earnings_per_hr": integer("INR/h for one more driver here"), "supply": integer("Idle cars in the cell"),
			"recent_requests": integer("Last 30 min"), "trend": enum("", "rising", "steady", "falling"), "history_rate": num(""),
			"explanation": str("One line for drivers"), "explanation_source": enum("", "template", "llm"),
		}),
		"ForecastHeatmap": obj([]string{"horizon", "cells"}, M{
			"as_of": dt(""), "horizon": integer("0, 15 or 30"), "resolution": integer(""), "model": str(""), "source": str(""),
			"scenarios": arr(str("")), "runs": integer(""), "interval_s": num(""), "hotspots": arr(str("Top cells by earnings")),
			"cells": arr(ref("ForecastCell")),
		}),
		"QuoteInput": obj(nil, M{
			"tier": withDesc(tier, "Default mini"), "pickup": ref("LatLng"), "dropoff": ref("LatLng"),
			"distance_km": num("With duration_min, instead of pickup/dropoff"), "duration_min": num(""),
			"supply": integer("Override cell supply"), "demand": integer("Override cell demand"),
		}),
		"Quote": obj([]string{"tier", "currency", "total"}, M{
			"tier": str(""), "tier_label": str(""), "currency": str(""), "distance_km": num(""), "duration_min": num(""),
			"base": num(""), "distance_fare": num(""), "time_fare": num(""), "minimum_fare": num(""), "minimum_applied": boolean(""),
			"subtotal": num(""), "surge_multiplier": num(""), "surge_amount": num(""), "surge_model": str(""),
			"cell": anyObj(""), "total": num("INR, rounded"), "eta_source": str(""),
		}),

		// ---- legal & jurisdiction ----
		"LegalIngest": obj([]string{"text"}, M{"city": str("bengaluru | mumbai | delhi (or an alias); inferred from the text when omitted"),
			"text": str("Regulation text, 1..20000 chars")}),
		"LegalRules": obj([]string{"max_surge_multiplier", "min_insurance_coverage", "max_driving_hours", "av_allowed_zones", "required_documents", "city_levy_pct"}, M{
			"max_surge_multiplier": num("Surge cap, x base fare"), "min_insurance_coverage": integer("INR"),
			"max_driving_hours": integer("Per driver per 24 h"), "av_allowed_zones": arr(str("H3 cell")),
			"required_documents": arr(str("Document key")), "city_levy_pct": num("Percent of the fare"),
		}),
		"Jurisdiction": obj([]string{"city", "name", "rules", "version"}, M{
			"city": str(""), "name": str(""), "state": orNull(str("")), "authority": orNull(str("")), "rules": ref("LegalRules"),
			"version": integer(""), "updated_at": orNull(str("")), "updated_by": orNull(str("")),
		}),
		"Jurisdictions": obj([]string{"enabled", "jurisdictions"}, M{
			"enabled": boolean("LEGAL_ENABLED"), "jurisdictions": arr(ref("Jurisdiction")), "backend": str("postgres | memory"),
			"pricing_city": orNull(str("")), "sample_notice": str("Sample regulation for the Ops console"),
		}),
		"RuleDiff": obj([]string{"field", "new"}, M{"field": str(""), "old": M{}, "new": M{}}),
		"LegalIngestResult": obj([]string{"city", "jurisdiction", "diff", "applied", "audit_id", "source"}, M{
			"city": str(""), "jurisdiction": ref("Jurisdiction"), "diff": arr(ref("RuleDiff")),
			"rejected": arr(anyObj("{field, value, reason}")), "overridden": arr(str("Fields where the text overrode the LLM")),
			"applied": boolean(""), "audit_id": str(""), "source": enum("", "llm", "rules"), "model": str(""),
			"used_fallback": boolean(""), "fallback_reason": orNull(str("")), "summary": str(""), "event_id": M{},
		}),

		// ---- ops report ----
		"OpsReport": obj([]string{"id", "generated_at", "trigger", "kpis", "anomalies", "actions", "human_interventions", "summary", "markdown"}, M{
			"id": str(""), "generated_at": str("RFC 3339"), "trigger": enum("", "schedule", "manual", "on_demand"),
			"kpis": anyObj("Headline KPIs"), "agents": arr(anyObj("")), "incidents": anyObj(""), "money": orNull(anyObj("")),
			"anomalies": arr(anyObj("{kind, subject, severity, detail}")), "actions": arr(anyObj("{kind, subject, action, result}")),
			"human_interventions": integer("Always 0"), "summary": str(""), "summary_source": enum("", "template", "llm"),
			"sources": mapOf(str("")), "markdown": str("The rendered report"),
		}),
		"OpsReportRow": obj([]string{"id", "generated_at"}, M{
			"id": str(""), "generated_at": str(""), "trigger": str(""), "anomalies": integer(""), "summary_source": str(""),
			"human_interventions": integer(""), "headline": str(""),
		}),

		// ---- take-over mode ----
		"RideOffer": obj([]string{"type", "ride_id", "driver_id"}, M{
			"type": enum("", "ride_offer", "offer_closed"), "ride_id": str(""), "driver_id": str(""), "rider_id": str(""),
			"pickup": ref("LatLng"), "dropoff": ref("LatLng"), "fare_inr": num(""), "trip_km": num(""),
			"pickup_eta_sec": integer(""), "timeout_s": integer(""), "expires_at": dt(""),
			"reason": enum("offer_closed only", "accepted", "declined", "timeout", "cancelled", "released"),
		}),
		"TakeoverView": obj([]string{"driver_id", "human_controlled"}, M{
			"driver_id": str(""), "human_controlled": boolean(""), "expires_at": dt(""), "ttl_s": integer(""), "offer": ref("RideOffer"),
		}),
		"OfferResult": obj([]string{"ride_id", "decision"}, M{
			"ride_id": str(""), "driver_id": str(""), "decision": enum("", "accepted", "declined"), "status": str("Ride status afterwards"),
			"rematched_to": str("Declined: the new driver, if any"),
		}),

		// ---- developer platform ----
		"Scope":        enum("", "rides:read", "rides:write", "pricing:read", "stream:read", "agents:invoke", "ledger:read"),
		"WebhookEvent": enum("", "ride.completed", "incident.opened", "payout.sent"),
		"Scopes": obj([]string{"scopes", "events"}, M{
			"scopes": mapOf(str("")), "events": arr(ref("WebhookEvent")), "default_rate_limit_per_min": integer(""), "signature": anyObj(""),
		}),
		"CreateKey": obj(nil, M{
			"name": str("Up to 80 characters"), "scopes": arr(ref("Scope")), "rate_limit_per_min": integer("1..600, default 60"),
		}),
		"Key": obj([]string{"id", "name", "prefix", "scopes", "rate_limit_per_min", "created_at"}, M{
			"id": str(""), "name": str(""), "prefix": str("First characters of the key"), "scopes": arr(ref("Scope")),
			"rate_limit_per_min": integer(""), "created_at": dt(""), "revoked_at": dt(""),
		}),
		"CreatedKey": M{"allOf": []M{ref("Key"), obj([]string{"api_key"}, M{
			"api_key": str("hxg_…: shown only in this response"), "warning": str(""),
		})}},
		"CreateWebhook": obj([]string{"url", "events"}, M{"url": str("Public http(s) URL"), "events": arr(ref("WebhookEvent"))}),
		"Webhook": obj([]string{"id", "url", "events", "created_at"}, M{
			"id": str(""), "key_id": str(""), "url": str(""), "events": arr(ref("WebhookEvent")), "created_at": dt(""),
		}),
		"CreatedWebhook": M{"allOf": []M{ref("Webhook"), obj([]string{"secret"}, M{
			"secret": str("whsec_…: HMAC signing secret, shown only in this response"),
		})}},
		"WebhookList": obj([]string{"webhooks"}, M{"webhooks": arr(ref("Webhook")), "count": integer("")}),
		"Delivery": obj([]string{"event_id", "event", "attempts", "ok"}, M{
			"event_id": str(""), "event": str(""), "webhook_id": str(""), "attempts": integer("1 + up to 3 retries"),
			"status_code": integer(""), "ok": boolean(""), "error": str(""), "at": dt(""),
		}),
		"DeliveryList": obj([]string{"deliveries"}, M{"deliveries": arr(ref("Delivery")), "count": integer("")}),
		"WebhookPayload": obj([]string{"id", "type", "created_at", "data"}, M{
			"id": str("evt_…, stable across retries (X-HexaGo-Delivery)"), "type": str(""), "created_at": dt(""),
			"data": M{"description": "ride.completed: Ride; incident.opened: incident summary; payout.sent: Payout"},
		}),
	}
}
