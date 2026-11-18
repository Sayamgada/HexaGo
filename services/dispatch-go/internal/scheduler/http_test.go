package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

func router(s *Scheduler) *chi.Mux {
	r := chi.NewRouter()
	s.Routes(r)
	return r
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestScheduledEndpoints(t *testing.T) {
	s, _, _, _ := newSched(true)
	h := router(s)
	body, _ := json.Marshal(goldenPlan())
	code, out := do(t, h, "POST", "/v1/scheduled", string(body))
	if code != http.StatusCreated || out["plan_id"] != "pln_golden" || out["demo_mode"] != true {
		t.Fatalf("POST: %d %v", code, out)
	}
	if legs := out["legs"].([]any); len(legs) != 2 || legs[0].(map[string]any)["status"] != "scheduled" {
		t.Fatalf("legs: %v", legs)
	}
	code, out = do(t, h, "GET", "/v1/scheduled?rider_id=rider_priya", "")
	if code != 200 || out["count"].(float64) != 2 || out["server_time"] == "" {
		t.Fatalf("GET: %d %v", code, out)
	}
	if _, out = do(t, h, "GET", "/v1/scheduled?plan_id=pln_golden", ""); out["count"].(float64) != 2 {
		t.Fatalf("by plan: %v", out)
	}
	if _, out = do(t, h, "GET", "/v1/scheduled?rider_id=nobody", ""); out["count"].(float64) != 0 || out["legs"] == nil {
		t.Fatalf("empty: %v", out)
	}
	if code, _ = do(t, h, "POST", "/v1/scheduled", "{"); code != 400 {
		t.Fatalf("bad json = %d", code)
	}
	if code, _ = do(t, h, "POST", "/v1/scheduled", `{"rider_id":"r","legs":[]}`); code != 400 {
		t.Fatalf("no legs = %d", code)
	}
}

func TestETAEndpoint(t *testing.T) {
	s, _, _, _ := newSched(true)
	h := router(s)
	code, out := do(t, h, "GET", "/v1/eta?from_lat=13.0475&from_lng=77.62&to_lat=13.1989&to_lng=77.7068", "")
	if code != 200 || out["source"] != "dispatch-go" {
		t.Fatalf("eta: %d %v", code, out)
	}
	km, min := out["distance_km"].(float64), out["duration_min"].(float64)
	if km < 24 || km > 26 || min < 45 || min > 52 {
		t.Fatalf("office -> airport %.2f km %.1f min", km, min)
	}
	for _, q := range []string{"", "?from_lat=1&from_lng=2&to_lat=3", "?from_lat=x&from_lng=2&to_lat=3&to_lng=4",
		"?from_lat=95&from_lng=2&to_lat=3&to_lng=4"} {
		if code, _ := do(t, h, "GET", "/v1/eta"+q, ""); code != 400 {
			t.Errorf("%s -> %d", q, code)
		}
	}
}

func TestTripModelMatchesPython(t *testing.T) {
	cases := map[float64]float64{0.1: 3, 3: 10, 6: 20, 26: 50}
	for km, want := range cases {
		if got := DurationForRoadKm(km); got != want {
			t.Errorf("%v km -> %v min, want %v", km, got, want)
		}
	}
	trip := EstimateTrip(geo.Point{Lat: 12.97, Lng: 77.59}, geo.Point{Lat: 12.97, Lng: 77.59})
	if trip.DistanceKm != 0 || trip.DurationS != 180 {
		t.Fatalf("zero trip %+v", trip)
	}
}

func TestSupplyEndpoint(t *testing.T) {
	center := geo.Point{Lat: 12.9716, Lng: 77.5946}
	ix, err := geo.NewIndexer(9, center)
	if err != nil {
		t.Fatal(err)
	}
	idx := fleet.New(ix)
	add := func(id string, st fleet.Status, dLat float64) {
		if _, err := idx.Upsert(fleet.Driver{ID: id, Lat: center.Lat + dLat, Lng: center.Lng, Status: st,
			Tier: fleet.TierMini, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	add("idle1", fleet.StatusIdle, 0)
	add("busy1", fleet.StatusEnRoute, 0)
	add("busy2", fleet.StatusOnTrip, 0.0005)
	add("off", fleet.StatusOffline, 0)
	add("far", fleet.StatusIdle, 0.2)
	s := New(Options{Rides: newRides(), Geo: ix, Fleet: idx, Logger: quiet()})
	h := router(s)
	code, out := do(t, h, "GET", "/v1/supply?lat=12.9716&lng=77.5946", "")
	if code != 200 || out["supply"].(float64) != 1 || out["demand"].(float64) != 2 || out["cell"] == "" ||
		out["rings"].(float64) != 1 {
		t.Fatalf("supply: %d %v", code, out)
	}
	if _, out = do(t, h, "GET", "/v1/supply?lat=12.9716&lng=77.5946&rings=0", ""); out["cells"].(float64) != 1 {
		t.Fatalf("rings=0: %v", out)
	}
	if code, _ = do(t, h, "GET", "/v1/supply?lat=12.97", ""); code != 400 {
		t.Fatalf("missing lng = %d", code)
	}
	if code, _ = do(t, h, "GET", "/v1/supply?lat=99&lng=77", ""); code != 400 {
		t.Fatalf("bad lat = %d", code)
	}
	noFleet, _, _, _ := newSched(true)
	if code, _ = do(t, router(noFleet), "GET", "/v1/supply?lat=12.97&lng=77.59", ""); code != 503 {
		t.Fatalf("no fleet = %d", code)
	}
}

// ---- Postgres repo against a fake pgx DB ----

type fakeDB struct {
	execs   []string
	args    [][]any
	rows    [][]byte
	execErr error
	qErr    error
	scanErr error
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	f.args = append(f.args, args)
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if f.qErr != nil {
		return nil, f.qErr
	}
	return &fakeRows{data: f.rows, scanErr: f.scanErr}, nil
}

type fakeRows struct {
	data    [][]byte
	i       int
	scanErr error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool                                   { r.i++; return r.i <= len(r.data) }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map                         { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*(dest[0].(*[]byte)) = r.data[r.i-1]
	return nil
}

func TestPGRepo(t *testing.T) {
	db := &fakeDB{}
	repo := NewPGRepo(db)
	ctx := context.Background()
	l := Leg{ID: "sch_1", PlanID: "p", RiderID: "r", Seq: 1, Status: StatusDispatched, RideID: "ride-1", PickupAt: t0}
	if err := repo.Save(ctx, l); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.execs[0], "INSERT INTO scheduled_legs") || !strings.Contains(db.execs[0], "ON CONFLICT (id)") {
		t.Fatalf("sql: %s", db.execs[0])
	}
	if db.args[0][0] != "sch_1" || db.args[0][4] != "dispatched" {
		t.Fatalf("args: %v", db.args[0])
	}
	raw, _ := json.Marshal(l)
	db.rows = [][]byte{raw}
	got, err := repo.Load(ctx)
	if err != nil || len(got) != 1 || got[0].RideID != "ride-1" {
		t.Fatalf("load: %v %v", got, err)
	}
	db.rows = [][]byte{[]byte("{")}
	if _, err := repo.Load(ctx); err == nil {
		t.Fatal("bad json accepted")
	}
	db.scanErr = errors.New("scan")
	if _, err := repo.Load(ctx); err == nil {
		t.Fatal("scan error swallowed")
	}
	db.qErr = errors.New("down")
	if _, err := repo.Load(ctx); err == nil {
		t.Fatal("query error swallowed")
	}
	db.execErr = errors.New("down")
	if err := repo.Save(ctx, l); err == nil {
		t.Fatal("exec error swallowed")
	}
	if !bytes.Contains(raw, []byte(`"plan_id":"p"`)) {
		t.Fatal("json shape")
	}
}
