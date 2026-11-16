package fleet

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestRedisMirrorIntegration runs against a real Redis when TEST_REDIS_URL is set.
func TestRedisMirrorIntegration(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis integration test")
	}
	ctx := context.Background()
	m, err := NewRedisMirror(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.key = "fleet:test:" + t.Name()
	defer m.client.Del(ctx, m.key)

	idx, _ := newTestIndex(t, WithMirror(m))
	if _, err := idx.Upsert(driver("redis-d1", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}
	res, err := m.client.GeoSearch(ctx, m.key, &redis.GeoSearchQuery{
		Longitude: center.Lng, Latitude: center.Lat, Radius: 100, RadiusUnit: "m",
	}).Result()
	if err != nil || len(res) != 1 || res[0] != "redis-d1" {
		t.Fatalf("GEOSEARCH = %v, %v", res, err)
	}
	idx.Remove("redis-d1")
	if n, _ := m.client.ZCard(ctx, m.key).Result(); n != 0 {
		t.Errorf("ZCARD after remove = %d", n)
	}
}
