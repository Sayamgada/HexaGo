package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

func TestDriverRoutesFallBackToFleetAndShowSuspension(t *testing.T) {
	sf := safety.New(safety.Options{Config: safety.DefaultConfig(true)})
	router := server.NewRouter(testConfig(), server.WithSafety(sf))
	do := func(method, path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		_ = json.NewDecoder(rec.Body).Decode(&out)
		return rec.Code, out
	}
	for _, id := range []string{"drv-0001", "drv-0002"} {
		tier := "sedan"
		if id == "drv-0002" {
			tier = "av"
		}
		if code, out := do("POST", "/v1/drivers/"+id+"/status", `{"lat":12.97,"lng":77.59,"status":"idle","tier":"`+tier+`"}`); code != 200 {
			t.Fatalf("seed %s: %d %v", id, code, out)
		}
	}
	code, out := do("GET", "/v1/drivers?tier=av", "")
	if code != 200 || out["count"].(float64) != 1 || out["simulated"] != false {
		t.Fatalf("list: %d %v", code, out)
	}
	code, out = do("GET", "/v1/drivers?q=0001", "")
	if code != 200 || out["count"].(float64) != 1 {
		t.Fatalf("search: %d %v", code, out)
	}
	if code, _ = do("GET", "/v1/drivers/missing", ""); code != http.StatusNotFound {
		t.Fatalf("missing: %d", code)
	}
	if _, err := sf.Suspend(context.Background(), "drv-0001", "inc-42", "test", "safety_sentinel"); err != nil {
		t.Fatal(err)
	}
	code, out = do("GET", "/v1/drivers/drv-0001", "")
	if code != 200 || out["suspended"] != true || out["incident_id"] != "inc-42" || out["status"] != "suspended" {
		t.Fatalf("suspended detail: %d %v", code, out)
	}
}
