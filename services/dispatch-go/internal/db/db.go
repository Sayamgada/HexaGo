package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Clients holds database and cache connection pools.
type Clients struct {
	PG    *pgxpool.Pool
	Redis *redis.Client
}

// Connect creates connections to PostgreSQL and Redis.
func Connect(ctx context.Context, pgURL, redisURL string) (*Clients, error) {
	pgConfig, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse postgres config: %w", err)
	}
	pgConfig.MaxConns = 25
	pgConfig.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, pgConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create postgres pool: %w", err)
	}

	rOpt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse redis url: %w", err)
	}
	rdb := redis.NewClient(rOpt)

	return &Clients{
		PG:    pool,
		Redis: rdb,
	}, nil
}

// Close gracefully releases connections.
func (c *Clients) Close() {
	if c.PG != nil {
		c.PG.Close()
	}
	if c.Redis != nil {
		_ = c.Redis.Close()
	}
}

// Ping checks connectivity with a timeout.
func (c *Clients) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if c.PG != nil {
		if err := c.PG.Ping(ctx); err != nil {
			return fmt.Errorf("postgres ping failed: %w", err)
		}
	}
	if c.Redis != nil {
		if err := c.Redis.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis ping failed: %w", err)
		}
	}
	return nil
}
