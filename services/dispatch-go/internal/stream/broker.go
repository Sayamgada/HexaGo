package stream

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Broker carries encoded messages between gateway instances. Every instance
// subscribed to the broker (including the publisher) receives each message.
type Broker interface {
	Kind() string
	Publish(ctx context.Context, msg []byte) error
	// Subscribe calls fn for every message until ctx is cancelled.
	Subscribe(ctx context.Context, fn func(msg []byte)) error
	Close() error
}

// LocalBroker fans messages out within this process.
type LocalBroker struct {
	mu   sync.RWMutex
	subs map[int]func([]byte)
	next int
}

// NewLocalBroker returns an in-process broker.
func NewLocalBroker() *LocalBroker { return &LocalBroker{subs: map[int]func([]byte){}} }

// Kind implements Broker.
func (b *LocalBroker) Kind() string { return "local" }

// Publish implements Broker. Delivery is synchronous.
func (b *LocalBroker) Publish(_ context.Context, msg []byte) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, fn := range b.subs {
		fn(msg)
	}
	return nil
}

// Subscribe implements Broker.
func (b *LocalBroker) Subscribe(ctx context.Context, fn func([]byte)) error {
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = fn
	b.mu.Unlock()
	<-ctx.Done()
	b.mu.Lock()
	delete(b.subs, id)
	b.mu.Unlock()
	return nil
}

// Subscribers returns the number of active subscriptions.
func (b *LocalBroker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Close implements Broker.
func (b *LocalBroker) Close() error { return nil }

// RedisChannel is the pub/sub channel shared by all gateway instances.
const RedisChannel = "hexago:stream"

// RedisBroker uses Redis pub/sub so every gateway instance sees every message.
type RedisBroker struct {
	client  *redis.Client
	channel string
}

// NewRedisBroker connects to url and verifies it with PING.
func NewRedisBroker(ctx context.Context, url string) (*RedisBroker, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("stream: parse REDIS_URL: %w", err)
	}
	opt.DialTimeout = 2 * time.Second
	c := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.Ping(pctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("stream: redis ping: %w", err)
	}
	return &RedisBroker{client: c, channel: RedisChannel}, nil
}

// Kind implements Broker.
func (b *RedisBroker) Kind() string { return "redis" }

// Publish implements Broker.
func (b *RedisBroker) Publish(ctx context.Context, msg []byte) error {
	return b.client.Publish(ctx, b.channel, msg).Err()
}

// Subscribe implements Broker. go-redis reconnects the subscription on its own.
func (b *RedisBroker) Subscribe(ctx context.Context, fn func([]byte)) error {
	ps := b.client.Subscribe(ctx, b.channel)
	defer ps.Close()
	ch := ps.Channel(redis.WithChannelSize(256))
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-ch:
			if !ok {
				return nil
			}
			fn([]byte(m.Payload))
		}
	}
}

// Close implements Broker.
func (b *RedisBroker) Close() error { return b.client.Close() }
