package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisGeoKey is the Redis GEO sorted set holding active driver positions.
const RedisGeoKey = "fleet:drivers:geo"

// RedisMirror mirrors active driver positions into a Redis GEO set so other
// services can run GEOSEARCH without calling the gateway.
type RedisMirror struct {
	client *redis.Client
	key    string
}

// NewRedisMirror connects to url and verifies the connection with PING.
func NewRedisMirror(ctx context.Context, url string) (*RedisMirror, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("fleet: parse REDIS_URL: %w", err)
	}
	opt.DialTimeout = 2 * time.Second
	client := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("fleet: redis ping: %w", err)
	}
	return &RedisMirror{client: client, key: RedisGeoKey}, nil
}

// Set implements Mirror.
func (m *RedisMirror) Set(ctx context.Context, d Driver) error {
	return m.client.GeoAdd(ctx, m.key, &redis.GeoLocation{Name: d.ID, Longitude: d.Lng, Latitude: d.Lat}).Err()
}

// SetMany implements BatchMirror with a single GEOADD.
func (m *RedisMirror) SetMany(ctx context.Context, ds []Driver) error {
	if len(ds) == 0 {
		return nil
	}
	locs := make([]*redis.GeoLocation, len(ds))
	for i, d := range ds {
		locs[i] = &redis.GeoLocation{Name: d.ID, Longitude: d.Lng, Latitude: d.Lat}
	}
	return m.client.GeoAdd(ctx, m.key, locs...).Err()
}

// Remove implements Mirror.
func (m *RedisMirror) Remove(ctx context.Context, id string) error {
	return m.client.ZRem(ctx, m.key, id).Err()
}

// Close releases the Redis connection pool.
func (m *RedisMirror) Close() error { return m.client.Close() }
