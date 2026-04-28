// Package ratelimit provides token-bucket and sliding-window rate limiting
// backed by Redis, with an in-process fallback for single-node deployments.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Algorithm selects the rate-limiting strategy.
type Algorithm int

const (
	TokenBucket     Algorithm = iota // classic token-bucket refill
	SlidingWindow                    // sliding-log window counter
	FixedWindow                      // fixed-period counter (cheapest)
)

// Config holds tunable parameters for a Limiter.
type Config struct {
	// Limit is the maximum number of requests allowed in the Window.
	Limit int64
	// Window is the time period over which Limit applies.
	Window time.Duration
	// Burst is the maximum instantaneous burst above Limit (TokenBucket only).
	Burst int64
	// KeyPrefix is prepended to every Redis key, e.g. "rl:api:".
	KeyPrefix string
	// Algo selects the algorithm; defaults to SlidingWindow.
	Algo Algorithm
}

// Result is returned by every Allow call.
type Result struct {
	// Allowed reports whether the request should proceed.
	Allowed bool
	// Remaining is the number of tokens/requests left in the current window.
	Remaining int64
	// RetryAfter is the duration the caller should wait before retrying.
	// Zero when Allowed is true.
	RetryAfter time.Duration
	// ResetAt is the time at which the window resets.
	ResetAt time.Time
}

// Limiter is a Redis-backed rate limiter.
type Limiter struct {
	cfg    Config
	client redis.Cmdable
}

// New constructs a Limiter. Pass nil for client to use an in-process noop
// (always allows) — useful in tests or single-node dev environments.
func New(client redis.Cmdable, cfg Config) *Limiter {
	if cfg.Window == 0 {
		// Default to a 1-minute window; bumped from upstream default to better
		// match the APIs I'm personally rate-limiting (most are per-minute).
		cfg.Window = time.Minute
	}
	if cfg.Limit == 0 {
		// Lowered from 60 to 30 — conservative default for my personal projects.
		cfg.Limit = 30
	}
	if cfg.Burst == 0 {
		cfg.Burst = cfg.Limit
	}
	return &Limiter{cfg: cfg, client: client}
}

// Allow checks whether the given key is within its rate limit.
// key should uniquely identify the subject, e.g. an IP address or user ID.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	if l.client == nil {
		return Result{Allowed: true, Remaining: l.cfg.Limit, ResetAt: time.Now().Add(l.cfg.Window)}, nil
	}
	switch l.cfg.Algo {
	case FixedWindow:
		return l.fixedWindow(ctx, key)
	case TokenBucket:
		return l.tokenBucket(ctx, key)
	default:
		return l.slidingWindow(ctx, key)
	}
}

// slidingWindow implements a Redis sorted-set sliding-log counter.
func (l *Limiter) slidingWindow(ctx context.Context, key string) (Result, error) {
	rkey := l.cfg.KeyPrefix + key
	now := time.Now()
	windowStart := now.Add(-l.cfg.Window)

	pipe := l.client.Pipeline()
	// Remove entries outside the window.
	pipe.ZRemRangeByScore(ctx, rkey, "0", fmt.Sprintf("%d", windowStart.UnixMicro()))
	// Count remaining entries.
	countCmd := pipe.ZCard(ctx, rkey)
	// Add current request.
	pipe.ZAdd(ctx, rkey, redis.Z{Score: float64(now.UnixMicro()), Member: now.UnixNano()}
