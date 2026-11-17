package redisdiag_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/hexago/hexago/services/dispatch-go/internal/redisdiag"
)

func TestDescribeMasksPassword(t *testing.T) {
	cases := []struct {
		raw, masked, scheme, host, port string
		tls                             bool
		db                              int
	}{
		{"redis://red-abc123:6379", "redis://red-abc123:6379", "redis", "red-abc123", "6379", false, 0},
		{"rediss://red-abc:s3cr3t@oregon-keyvalue.render.com:6379/2", "rediss://red-abc:****@oregon-keyvalue.render.com:6379/2",
			"rediss", "oregon-keyvalue.render.com", "6379", true, 2},
		{"redis://:pw@localhost", "redis://:****@localhost", "redis", "localhost", "6379", false, 0},
	}
	for _, c := range cases {
		info, err := redisdiag.Describe(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if info.URL != c.masked || info.Scheme != c.scheme || info.Host != c.host || info.Port != c.port ||
			info.TLS != c.tls || info.DB != c.db {
			t.Errorf("%s: got %+v", c.raw, info)
		}
		if strings.Contains(info.URL, "s3cr3t") || strings.Contains(info.URL, "pw@") {
			t.Errorf("password leaked: %s", info.URL)
		}
	}
	if info, _ := redisdiag.Describe("redis://red-abc:6379"); info.Internal != true {
		t.Errorf("red-* host without a domain should be flagged internal: %+v", info)
	}
	if _, err := redisdiag.Describe("http://x"); err == nil {
		t.Error("non-redis scheme should fail")
	}
	if _, err := redisdiag.Describe("::"); err == nil {
		t.Error("garbage should fail")
	}
}

func TestCheckRoundTripOK(t *testing.T) {
	mr := miniredis.RunT(t)
	res := redisdiag.Check(context.Background(), "redis://"+mr.Addr()+"/0", 2*time.Second)
	if !res.OK || res.FailedStage != "" || res.Error != "" {
		t.Fatalf("expected ok, got %+v", res)
	}
	if !res.Configured || res.PingMs < 0 || res.PubSubMs < 0 || res.CheckedAt == "" {
		t.Errorf("bad result %+v", res)
	}
}

func TestCheckAuthFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("right")
	res := redisdiag.Check(context.Background(), "redis://:wrong@"+mr.Addr(), 2*time.Second)
	if res.OK || res.FailedStage != "ping" || !strings.Contains(res.Hint, "password") {
		t.Fatalf("got %+v", res)
	}
	if strings.Contains(res.URL, "wrong") {
		t.Errorf("password leaked: %s", res.URL)
	}
}

func TestCheckUnreachable(t *testing.T) {
	res := redisdiag.Check(context.Background(), "redis://127.0.0.1:1", time.Second)
	if res.OK || res.FailedStage != "ping" || res.Hint == "" {
		t.Fatalf("got %+v", res)
	}
}

func TestCheckTLSAgainstPlainServer(t *testing.T) {
	mr := miniredis.RunT(t)
	res := redisdiag.Check(context.Background(), "rediss://"+mr.Addr(), 2*time.Second)
	if res.OK || !strings.Contains(strings.ToLower(res.Hint), "tls") {
		t.Fatalf("got %+v", res)
	}
}

func TestCheckNotConfiguredAndBadURL(t *testing.T) {
	if res := redisdiag.Check(context.Background(), "", time.Second); res.Configured || res.OK {
		t.Errorf("got %+v", res)
	}
	if res := redisdiag.Check(context.Background(), "http://x", time.Second); res.FailedStage != "parse" {
		t.Errorf("got %+v", res)
	}
}

func TestHints(t *testing.T) {
	cases := map[string]string{
		"dial tcp: lookup red-abc on 10.0.0.1:53: no such host": "region",
		"dial tcp 1.2.3.4:6379: connect: connection refused":    "refused",
		"EOF": "TLS",
		"tls: first record does not look like a TLS handshake": "redis://",
		"x509: certificate signed by unknown authority":        "certificate",
		"WRONGPASS invalid username-password pair":             "password",
		"NOAUTH Authentication required.":                      "password",
		"dial tcp 1.2.3.4:6379: i/o timeout":                   "allow list",
		"context deadline exceeded":                            "allow list",
		"something else":                                       "",
	}
	for msg, want := range cases {
		got := redisdiag.Hint(errors.New(msg))
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("Hint(%q) = %q, want it to mention %q", msg, got, want)
		}
	}
	if redisdiag.Hint(nil) != "" {
		t.Error("nil error has no hint")
	}
}

func TestMonitorCachesAndRefreshes(t *testing.T) {
	mr := miniredis.RunT(t)
	m := redisdiag.NewMonitor("redis://"+mr.Addr(), 20*time.Millisecond, time.Second)
	if m.Last().CheckedAt != "" {
		t.Fatal("no check before Run/CheckNow")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !m.Last().OK && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !m.Last().OK {
		t.Fatalf("monitor never succeeded: %+v", m.Last())
	}
	mr.Close()
	deadline = time.Now().Add(3 * time.Second)
	for m.Last().OK && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Last().OK {
		t.Fatal("monitor should notice Redis going away")
	}
	cancel()
	<-done
	if res := m.CheckNow(context.Background()); res.OK {
		t.Errorf("CheckNow against closed server: %+v", res)
	}
}
