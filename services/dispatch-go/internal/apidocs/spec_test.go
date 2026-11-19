package apidocs

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goldenPath is the committed copy SDK users and the Python contract tests read.
var goldenPath = filepath.Join("..", "..", "..", "..", "docs", "openapi.json")

// TestGoldenSpec keeps docs/openapi.json in sync with the served document.
// Regenerate with: UPDATE_OPENAPI=1 go test ./internal/apidocs -run TestGoldenSpec
func TestGoldenSpec(t *testing.T) {
	if os.Getenv("UPDATE_OPENAPI") == "1" {
		if err := os.WriteFile(goldenPath, JSON(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_OPENAPI=1)", goldenPath, err)
	}
	got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, JSON()) {
		t.Fatal("docs/openapi.json is stale: run UPDATE_OPENAPI=1 go test ./internal/apidocs -run TestGoldenSpec")
	}
}

func walkRefs(v any, fn func(string)) {
	switch x := v.(type) {
	case M:
		for k, val := range x {
			if k == "$ref" {
				fn(val.(string))
			}
			walkRefs(val, fn)
		}
	case []any:
		for _, it := range x {
			walkRefs(it, fn)
		}
	}
}

func TestSpecIsWellFormed(t *testing.T) {
	v := NewValidator()
	doc := v.Doc()
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", doc["openapi"])
	}
	walkRefs(doc, func(r string) {
		if v.resolve(r) == nil {
			t.Errorf("unresolved $ref %s", r)
		}
	})
	tags := map[string]bool{}
	for _, tg := range doc["tags"].([]any) {
		tags[tg.(M)["name"].(string)] = true
	}
	ids := map[string]string{}
	paramRe := regexp.MustCompile(`\{([^}]+)\}`)
	methods := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	for p, item := range doc["paths"].(M) {
		for method, raw := range item.(M) {
			if !methods[method] {
				t.Errorf("%s: unexpected key %s", p, method)
				continue
			}
			o := raw.(M)
			id, _ := o["operationId"].(string)
			if id == "" {
				t.Errorf("%s %s: no operationId", method, p)
			} else if prev, dup := ids[id]; dup {
				t.Errorf("operationId %s used by %s and %s %s", id, prev, method, p)
			}
			ids[id] = method + " " + p
			for _, tg := range o["tags"].([]any) {
				if !tags[tg.(string)] {
					t.Errorf("%s %s: unknown tag %v", method, p, tg)
				}
			}
			if len(o["responses"].(M)) == 0 {
				t.Errorf("%s %s: no responses", method, p)
			}
			declared := map[string]bool{}
			if ps, ok := o["parameters"].([]any); ok {
				for _, pr := range ps {
					pm := pr.(M)
					if pm["in"] == "path" {
						declared[pm["name"].(string)] = true
					}
				}
			}
			for _, m := range paramRe.FindAllStringSubmatch(p, -1) {
				if !declared[m[1]] {
					t.Errorf("%s %s: path parameter %s not declared", method, p, m[1])
				}
			}
			if len(declared) != len(paramRe.FindAllString(p, -1)) {
				t.Errorf("%s %s: declares path parameters not in the template", method, p)
			}
		}
	}
	if len(ids) < 60 {
		t.Errorf("only %d operations documented", len(ids))
	}
	for _, ev := range []string{"ride.completed", "incident.opened", "payout.sent"} {
		if _, ok := doc["webhooks"].(M)[ev]; !ok {
			t.Errorf("webhook %s not documented", ev)
		}
	}
}

// Every scoped operation names a scope the gateway knows.
func TestScopesInSpec(t *testing.T) {
	known := map[string]bool{sRidesR: true, sRidesW: true, sPricing: true, sStream: true, sAgents: true, sLedger: true}
	for p, item := range Spec()["paths"].(M) {
		for method, o := range item.(M) {
			if s, ok := o.(M)["x-required-scope"].(string); ok && !known[s] {
				t.Errorf("%s %s: unknown scope %s", method, p, s)
			}
		}
	}
}

func TestHandlers(t *testing.T) {
	rec := httptest.NewRecorder()
	SpecHandler(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("spec: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var doc M
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc["info"].(M)["title"] != "HexaGo Public API" {
		t.Fatalf("spec body: %v", err)
	}
	rec = httptest.NewRecorder()
	SwaggerHandler(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "swagger-ui-dist@5.17.14") ||
		!strings.Contains(rec.Body.String(), `url: "/openapi.json"`) {
		t.Fatalf("docs: %d %s", rec.Code, rec.Body.String())
	}
}

func TestValidator(t *testing.T) {
	v := NewValidator()
	ride := `{"ride":{"id":"r1","rider_id":"u","tier":"mini","status":"matched","pickup":{"lat":1,"lng":2},
		"dropoff":{"lat":1,"lng":2},"legs":null,"created_at":"2026-01-01T00:00:00Z"}}`
	if err := v.ValidateResponse("GET", "/v1/rides/{id}", 200, []byte(ride)); err != nil {
		t.Fatalf("valid ride rejected: %v", err)
	}
	bad := map[string]string{
		"missing required":  `{"ride":{"id":"r1"}}`,
		"bad enum":          strings.Replace(ride, `"matched"`, `"flying"`, 1),
		"bad type":          strings.Replace(ride, `"lat":1`, `"lat":"north"`, 1),
		"bad nested in arr": strings.Replace(ride, `"legs":null`, `"legs":[{"seq":"x"}]`, 1),
		"not json":          `{`,
	}
	for name, body := range bad {
		if err := v.ValidateResponse("GET", "/v1/rides/{id}", 200, []byte(body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
	// anyOf, allOf, nullable refs, maps.
	if err := v.ValidateResponse("GET", "/v1/ledger/entries", 200, []byte(`{"entries":[]}`)); err != nil {
		t.Error(err)
	}
	if err := v.ValidateResponse("GET", "/v1/ledger/entries", 200, []byte(`{"nope":1}`)); err == nil {
		t.Error("anyOf accepted an unrelated object")
	}
	if err := v.ValidateResponse("POST", "/v1/developer/keys", 201, []byte(`{"id":"k","name":"n","prefix":"hxg_1","scopes":["rides:read"],"rate_limit_per_min":60,"created_at":"x"}`)); err == nil {
		t.Error("allOf did not require api_key")
	}
	if err := v.ValidateResponse("GET", "/v1/pricing/tiers", 200, []byte(`{"currency":"INR","tiers":{"mini":{"base":"x"}}}`)); err == nil {
		t.Error("additionalProperties schema not enforced")
	}
	if err := v.Validate(M{"type": "object", "additionalProperties": false}, M{"x": 1.0}); err == nil {
		t.Error("additionalProperties false not enforced")
	}
	if err := v.Validate(M{"$ref": "#/components/schemas/Nope"}, 1.0); err == nil {
		t.Error("unresolved ref accepted")
	}
	if err := v.Validate(M{"$ref": "http://x"}, 1.0); err == nil {
		t.Error("remote ref accepted")
	}
	if err := v.Validate("weird", 1.0); err == nil {
		t.Error("non-object schema accepted")
	}
	if err := v.Validate(nil, 1.0); err != nil {
		t.Error(err)
	}
	if err := v.Validate(M{"type": "number"}, 1.5); err != nil {
		t.Error(err)
	}
	if err := v.Validate(M{"type": "integer"}, 1.5); err == nil {
		t.Error("1.5 accepted as integer")
	}
	if typeOf(struct{}{}) == "object" {
		t.Error("struct typed as object")
	}
	var deep any = M{"type": "object"}
	for i := 0; i < 70; i++ {
		deep = M{"allOf": []any{deep}}
	}
	if err := v.Validate(deep, M{}); err == nil {
		t.Error("runaway nesting accepted")
	}
	// Documentation lookups.
	if _, err := v.ResponseSchema("GET", "/nope", 200); err == nil {
		t.Error("unknown path")
	}
	if _, err := v.ResponseSchema("PUT", "/v1/rides", 200); err == nil {
		t.Error("unknown method")
	}
	if _, err := v.ResponseSchema("POST", "/v1/rides", 418); err == nil {
		t.Error("unknown status")
	}
	if s, err := v.ResponseSchema("GET", "/v1/rides/{id}", 401); err != nil || s == nil {
		t.Errorf("component response not resolved: %v %v", s, err)
	}
	if s, err := v.ResponseSchema("GET", "/metrics", 200); err != nil || s != nil {
		t.Errorf("non-JSON response: %v %v", s, err)
	}
	if err := v.ValidateResponse("GET", "/metrics", 200, []byte("# HELP")); err != nil {
		t.Errorf("non-JSON body validated: %v", err)
	}
}

func TestOrNull(t *testing.T) {
	v := NewValidator()
	s := orNull(enum("", "a"))
	for _, ok := range []any{"a", nil} {
		if err := v.Validate(roundTrip(s), ok); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	if err := v.Validate(roundTrip(s), "b"); err == nil {
		t.Error("b accepted")
	}
	r := orNull(ref("LatLng"))
	if err := v.Validate(roundTrip(r), nil); err != nil {
		t.Error(err)
	}
	if err := v.Validate(roundTrip(r), M{"lat": 1.0}); err == nil {
		t.Error("partial LatLng accepted")
	}
	if withDesc(num("a"), "b")["description"] != "b" || boolean("x")["description"] != "x" || integer("i")["description"] != "i" {
		t.Error("helpers")
	}
	for _, c := range []int{400, 404, 409, 413, 415, 422, 502, 503, 500} {
		if statusText(c) == "" {
			t.Error(c)
		}
	}
}

func roundTrip(s M) any {
	b, _ := json.Marshal(s)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
