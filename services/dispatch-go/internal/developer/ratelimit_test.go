package developer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// exerciseLimiter runs the same token-bucket behaviour against any limiter.
func exerciseLimiter(t *testing.T, l Limiter, clock *fakeClock) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		d, err := l.Allow(ctx, "k1", 60)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d: %+v %v", i, d, err)
		}
		if d.Limit != 60 || d.Remaining != 59-i {
			t.Fatalf("request %d: limit %d remaining %d", i, d.Limit, d.Remaining)
		}
	}
	d, _ := l.Allow(ctx, "k1", 60)
	if d.Allowed || d.Remaining != 0 || d.RetryAfter <= 0 || d.RetryAfter > time.Second+time.Millisecond {
		t.Fatalf("61st: %+v", d)
	}
	if d.Reset < 59*time.Second || d.Reset > 61*time.Second {
		t.Fatalf("reset %v", d.Reset)
	}
	// Another key has its own bucket.
	if d, _ := l.Allow(ctx, "k2", 60); !d.Allowed {
		t.Fatal("k2 limited by k1")
	}
	// One token per second refills.
	clock.Add(time.Second)
	if d, _ := l.Allow(ctx, "k1", 60); !d.Allowed {
		t.Fatalf("after 1 s: %+v", d)
	}
	if d, _ := l.Allow(ctx, "k1", 60); d.Allowed {
		t.Fatal("second request after 1 s allowed")
	}
	// A full minute refills to capacity, never beyond.
	clock.Add(5 * time.Minute)
	d, _ = l.Allow(ctx, "k1", 60)
	if !d.Allowed || d.Remaining != 59 {
		t.Fatalf("after refill: %+v", d)
	}
	// A clock step backwards does not mint tokens.
	clock.Add(-time.Hour)
	if d, _ := l.Allow(ctx, "k1", 60); !d.Allowed || d.Remaining != 58 {
		t.Fatalf("after clock skew: %+v", d)
	}
}

func TestMemoryLimiter(t *testing.T) {
	c := newClock()
	l := NewMemoryLimiter(c.Now)
	if l.Kind() != "memory" {
		t.Fatal(l.Kind())
	}
	exerciseLimiter(t, l, c)
	if NewMemoryLimiter(nil).now == nil {
		t.Fatal("default clock")
	}
}

func TestMemoryLimiterPrunesIdleBuckets(t *testing.T) {
	c := newClock()
	l := NewMemoryLimiter(c.Now)
	ctx := context.Background()
	for i := 0; i < 50001; i++ {
		l.b[string(rune(i))+"x"] = memBucket{tokens: 1, last: c.Now()}
	}
	c.Add(2 * time.Minute)
	_, _ = l.Allow(ctx, "fresh", 10)
	if len(l.b) != 1 {
		t.Fatalf("%d buckets left", len(l.b))
	}
}

func TestRedisLimiter(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newClock()
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()
	l := NewRedisLimiter(rc, c.Now)
	if l.Kind() != "redis" {
		t.Fatal(l.Kind())
	}
	exerciseLimiter(t, l, c)
	if ttl := mr.TTL("hexago:ratelimit:k1"); ttl <= 0 {
		t.Fatalf("bucket has no expiry: %v", ttl)
	}
	if NewRedisLimiter(rc, nil).now == nil {
		t.Fatal("default clock")
	}
}

func TestRedisLimiterFallsBackWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	defer rc.Close()
	l := NewRedisLimiter(rc, nil)
	mr.Close()
	d, err := l.Allow(context.Background(), "k", 2)
	if err == nil || !d.Allowed || d.Limit != 2 {
		t.Fatalf("fallback: %+v %v", d, err)
	}
	_, _ = l.Allow(context.Background(), "k", 2)
	if d, _ := l.Allow(context.Background(), "k", 2); d.Allowed {
		t.Fatal("fallback bucket does not limit")
	}
}

type badScripter struct{ redis.Scripter }

func (badScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	c := redis.NewCmd(ctx)
	c.SetVal([]any{int64(1)}) // malformed: one element
	return c
}

func TestRedisLimiterRejectsMalformedReply(t *testing.T) {
	l := NewRedisLimiter(badScripter{}, nil)
	d, err := l.Allow(context.Background(), "k", 5)
	if err == nil || !d.Allowed {
		t.Fatalf("malformed reply: %+v %v", d, err)
	}
}

type oddScripter struct{ redis.Scripter }

func (oddScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	c := redis.NewCmd(ctx)
	c.SetVal([]any{int64(0), "not-a-number"})
	return c
}

func TestRedisLimiterUnparsableTokens(t *testing.T) {
	d, err := NewRedisLimiter(oddScripter{}, nil).Allow(context.Background(), "k", 60)
	if err != nil || d.Allowed || d.Remaining != 0 || d.RetryAfter != time.Second {
		t.Fatalf("%+v %v", d, err)
	}
}
