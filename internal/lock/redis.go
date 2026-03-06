package lock

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"lineblocs.com/scheduler/internal/config"
)

// Client wraps a Redis client for distributed locking.
type Client struct {
	RDB *redis.Client
}

// NewClient creates a Redis client from config.
func NewClient(cfg *config.Config) (*Client, error) {
	opt, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("lock: parse redis URL: %w", err)
	}

	rdb := redis.NewClient(opt)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("lock: redis ping: %w", err)
	}

	return &Client{RDB: rdb}, nil
}

// Close closes the Redis client.
func (c *Client) Close() error {
	return c.RDB.Close()
}

// Acquire attempts to acquire a distributed lock with the given key and TTL.
// Returns true if the lock was acquired, false if it's already held.
func (c *Client) Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := c.RDB.SetNX(ctx, key, "running", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("lock: acquire %s: %w", key, err)
	}
	return ok, nil
}

// Release deletes the lock key.
func (c *Client) Release(ctx context.Context, key string) error {
	return c.RDB.Del(ctx, key).Err()
}

// SetDedupeKey sets a deduplication key with a TTL.
// Returns true if the key was new (not a duplicate).
func (c *Client) SetDedupeKey(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := c.RDB.SetNX(ctx, key, "true", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("lock: dedupe %s: %w", key, err)
	}
	return ok, nil
}
