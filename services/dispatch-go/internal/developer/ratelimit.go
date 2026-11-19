package developer

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultRateLimitPerMin is a new key's limit.
const DefaultRateLimitPerMin = 60

// Decision is one token-bucket check.
type Decision struct {
	Allowed    bool
	Limit      int           // bucket capacity (= requests per minute)
	Remaining  int           // whole tokens left after this request
	Reset      time.Duration // until the bucket is full again
	RetryAfter time.Duration // until the next token, when denied
}

// Limiter is a per-id token bucket of `limit` tokens refilled at limit per minute.
type Limiter interface {
	Kind() string
	Allow(ctx context.Context, id string, limit int) (Decision, error)
}

// bucket is the token-bucket arithmetic shared by both limiters.
// tokens/last are the stored state (tokens < 0 = new bucket).
func bucket(tokens float64, last, now time.Time, limit int) (Decision, float64) {
	capacity := float64(limit)
	rate := capacity / float64(time.Minute) // tokens per nanosecond
	if tokens < 0 {
		tokens = capacity
	} else if el := now.Sub(last); el > 0 {
		tokens = math.Min(capacity, tokens+float64(el)*rate)
	}
	d := Decision{Limit: limit}
	if tokens >= 1 {
		tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = time.Duration(math.Ceil((1 - tokens) / rate))
	}
	d.Remaining = int(math.Floor(tokens))
	d.Reset = time.Duration(math.Ceil((capacity - tokens) / rate))
	return d, tokens
}

// MemoryLimiter keeps buckets in process memory (no Redis, or Redis down).
type MemoryLimiter struct {
	mu  sync.Mutex
	now func() time.Time
	b   map[string]memBucket
}

type memBucket struct {
	tokens float64
	last   time.Time
}

// NewMemoryLimiter returns an in-process limiter; now may be nil.
func NewMemoryLimiter(now func() time.Time) *MemoryLimiter {
	if now == nil {
		now = time.Now
	}
	return &MemoryLimiter{now: now, b: map[string]memBucket{}}
}

// Kind implements Limiter.
func (m *MemoryLimiter) Kind() string { return "memory" }

// Allow implements Limiter.
func (m *MemoryLimiter) Allow(_ context.Context, id string, limit int) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	st, ok := m.b[id]
	tokens := -1.0
	if ok {
		tokens = st.tokens
	}
	d, left := bucket(tokens, st.last, now, limit)
	m.b[id] = memBucket{tokens: left, last: now}
	if len(m.b) > 50000 { // bound memory: drop buckets that are full again
		for k, v := range m.b {
			if now.Sub(v.last) > time.Minute {
				delete(m.b, k)
			}
		}
	}
	return d, nil
}

// tokenBucketLua atomically refills and takes one token. KEYS[1] = bucket;
// ARGV = capacity, refill rate (tokens/ms), now (ms). Returns {allowed, tokens}
// with tokens as a string (Lua numbers would be truncated to integers).
const tokenBucketLua = `
local cap = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local st = redis.call('HMGET', KEYS[1], 't', 'ts')
local tokens = tonumber(st[1])
local ts = tonumber(st[2])
if tokens == nil or ts == nil then
  tokens = cap
else
  local el = now - ts
  if el > 0 then tokens = math.min(cap, tokens + el * rate) end
end
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', KEYS[1], 't', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', KEYS[1], math.ceil(cap / rate) + 1000)
return {allowed, tostring(tokens)}
`

// RedisLimiter keeps buckets in Redis so every gateway instance shares them.
// When Redis errors it falls back to an in-memory limiter for that request.
type RedisLimiter struct {
	client   redis.Scripter
	script   *redis.Script
	now      func() time.Time
	fallback *MemoryLimiter
	prefix   string
}

// NewRedisLimiter returns a limiter on client; now may be nil.
func NewRedisLimiter(client redis.Scripter, now func() time.Time) *RedisLimiter {
	if now == nil {
		now = time.Now
	}
	return &RedisLimiter{client: client, script: redis.NewScript(tokenBucketLua), now: now,
		fallback: NewMemoryLimiter(now), prefix: "hexago:ratelimit:"}
}

// Kind implements Limiter.
func (r *RedisLimiter) Kind() string { return "redis" }

// Allow implements Limiter.
func (r *RedisLimiter) Allow(ctx context.Context, id string, limit int) (Decision, error) {
	now := r.now()
	capacity := float64(limit)
	ratePerMs := capacity / 60000
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	res, err := r.script.Run(ctx, r.client, []string{r.prefix + id},
		limit, strconv.FormatFloat(ratePerMs, 'g', -1, 64), now.UnixMilli()).Slice()
	if err != nil || len(res) != 2 {
		d, _ := r.fallback.Allow(ctx, id, limit)
		if err == nil {
			err = fmt.Errorf("developer: unexpected rate-limit reply %v", res)
		}
		return d, err
	}
	allowed, _ := res[0].(int64)
	s, _ := res[1].(string)
	tokens, perr := strconv.ParseFloat(s, 64)
	if perr != nil {
		tokens = 0
	}
	rate := capacity / float64(time.Minute)
	d := Decision{Allowed: allowed == 1, Limit: limit, Remaining: int(math.Floor(tokens)),
		Reset: time.Duration(math.Ceil((capacity - tokens) / rate))}
	if !d.Allowed {
		d.RetryAfter = time.Duration(math.Ceil((1 - tokens) / rate))
	}
	return d, nil
}
