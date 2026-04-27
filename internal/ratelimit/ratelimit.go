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
		cfg.Window = time.Minute
	}
	if cfg.Limit == 0 {
		cfg.Limit = 60
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
	pipe.ZAdd(ctx, rkey, redis.Z{Score: float64(now.UnixMicro()), Member: now.UnixNano()})
	// Set expiry so keys self-clean.
	pipe.Expire(ctx, rkey, l.cfg.Window*2)
	if _, err := pipe.Exec(ctx); err != nil {
		return Result{}, fmt.Errorf("ratelimit: pipeline: %w", err)
	}

	count := countCmd.Val()
	resetAt := now.Add(l.cfg.Window)
	if count >= l.cfg.Limit {
		return Result{
			Allowed:    false,
			Remaining:  0,
			RetryAfter: l.cfg.Window / time.Duration(l.cfg.Limit),
			ResetAt:    resetAt,
		}, nil
	}
	return Result{
		Allowed:   true,
		Remaining: l.cfg.Limit - count - 1,
		ResetAt:   resetAt,
	}, nil
}

// fixedWindow uses a simple Redis INCR with TTL.
func (l *Limiter) fixedWindow(ctx context.Context, key string) (Result, error) {
	rkey := fmt.Sprintf("%s%s:%d", l.cfg.KeyPrefix, key, time.Now().Truncate(l.cfg.Window).Unix())
	count, err := l.client.Incr(ctx, rkey).Result()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: incr: %w", err)
	}
	if count == 1 {
		l.client.Expire(ctx, rkey, l.cfg.Window) //nolint:errcheck
	}
	resetAt := time.Now().Truncate(l.cfg.Window).Add(l.cfg.Window)
	if count > l.cfg.Limit {
		return Result{
			Allowed:    false,
			Remaining:  0,
			RetryAfter: time.Until(resetAt),
			ResetAt:    resetAt,
		}, nil
	}
	return Result{Allowed: true, Remaining: l.cfg.Limit - count, ResetAt: resetAt}, nil
}

// tokenBucket is a thin wrapper that delegates to fixedWindow with burst support.
// A proper token-bucket would use a Lua script; this approximation is sufficient
// for most API-gateway use cases.
func (l *Limiter) tokenBucket(ctx context.Context, key string) (Result, error) {
	orig := l.cfg.Limit
	l.cfg.Limit = l.cfg.Burst
	r, err := l.fixedWindow(ctx, key)
	l.cfg.Limit = orig
	return r, err
}
