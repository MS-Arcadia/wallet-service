package redisx

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
)

// Rule is one rate-limit constraint: at most Limit events per Window.
type Rule struct {
	// Name identifies the rule in metrics and error details.
	Name string
	// Limit is the maximum number of events allowed in the window.
	Limit int64
	// Window is the sliding window length.
	Window time.Duration
}

// Decision is the outcome of a limiter check.
type Decision struct {
	// Allowed reports whether the event may proceed.
	Allowed bool
	// Rule is the rule that rejected the event, or "" when allowed.
	Rule string
	// Count is the number of events in the window, including this one.
	Count int64
	// Limit is the rejecting rule's limit.
	Limit int64
	// RetryAfter is how long until the oldest event leaves the window.
	RetryAfter time.Duration
}

// slidingWindowScript implements a sorted-set sliding window atomically.
//
// A Lua script is used because the check-then-record sequence must be atomic: two
// concurrent requests that both read "4 of 5 used" would both be allowed, and the
// limit would be exceeded. Redis executes a script single-threaded, which removes
// the race entirely.
//
// KEYS[1]   the sorted-set key
// ARGV[1]   now, in milliseconds
// ARGV[2]   window length, in milliseconds
// ARGV[3]   limit
// ARGV[4]   a unique member id for this event
// ARGV[5]   1 to record the event, 0 to only inspect
//
// Returns { allowed, count, oldest_score }
const slidingWindowScript = `
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]
local record = tonumber(ARGV[5])

-- Drop everything that has fallen out of the window.
redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)

local count = redis.call('ZCARD', key)
local allowed = 1
if count + record > limit then
  allowed = 0
end

if allowed == 1 and record == 1 then
  redis.call('ZADD', key, now, member)
  count = count + 1
end

-- Expire the key a little after the window so idle users leave nothing behind.
redis.call('PEXPIRE', key, window + 1000)

local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local oldestScore = now
if oldest[2] then
  oldestScore = tonumber(oldest[2])
end

return { allowed, count, oldestScore }
`

// Limiter enforces one or more sliding-window rules against a key.
type Limiter struct {
	client *Client
	script *redis.Script
	// prefix namespaces the keys, e.g. "giftcard-attempt".
	prefix string
	rules  []Rule
	// failOpen decides what happens when Redis is unreachable. Abuse detection
	// fails open — Redis being down must not stop a legitimate user from redeeming
	// a gift card — whereas a limiter protecting a scarce resource would fail closed.
	failOpen bool
}

// NewLimiter builds a Limiter. Rules are evaluated in order and the first
// violation wins, so list the tightest window first for the clearest message.
func NewLimiter(client *Client, prefix string, failOpen bool, rules ...Rule) *Limiter {
	return &Limiter{
		client:   client,
		script:   redis.NewScript(slidingWindowScript),
		prefix:   prefix,
		rules:    rules,
		failOpen: failOpen,
	}
}

// Allow records an event against key and reports whether it is permitted.
func (l *Limiter) Allow(ctx context.Context, key string) (Decision, error) {
	return l.evaluate(ctx, key, true)
}

// Peek reports whether an event would be permitted without recording it.
func (l *Limiter) Peek(ctx context.Context, key string) (Decision, error) {
	return l.evaluate(ctx, key, false)
}

func (l *Limiter) evaluate(ctx context.Context, key string, record bool) (Decision, error) {
	now := time.Now()
	// The member id must be unique per event or two events in the same millisecond
	// would collapse into one sorted-set entry and one of them would go uncounted.
	member := fmt.Sprintf("%d-%d", now.UnixNano(), rand.Int64())

	recordFlag := 0
	if record {
		recordFlag = 1
	}

	for _, rule := range l.rules {
		redisKey := fmt.Sprintf("ratelimit:%s:%s:%s", l.prefix, rule.Name, key)

		result, err := l.script.Run(ctx, l.client.Raw(),
			[]string{redisKey},
			now.UnixMilli(), rule.Window.Milliseconds(), rule.Limit, member, recordFlag,
		).Slice()
		if err != nil {
			if l.failOpen {
				return Decision{Allowed: true}, fmt.Errorf("redisx: rate limiter unavailable, failing open: %w", err)
			}
			return Decision{Allowed: false, Rule: rule.Name}, fmt.Errorf("redisx: rate limiter unavailable: %w", err)
		}

		allowed, count, oldest, err := parseDecision(result)
		if err != nil {
			if l.failOpen {
				return Decision{Allowed: true}, err
			}
			return Decision{Allowed: false, Rule: rule.Name}, err
		}

		if !allowed {
			retryAfter := time.Duration(oldest+rule.Window.Milliseconds()-now.UnixMilli()) * time.Millisecond
			if retryAfter < 0 {
				retryAfter = 0
			}
			return Decision{
				Allowed:    false,
				Rule:       rule.Name,
				Count:      count,
				Limit:      rule.Limit,
				RetryAfter: retryAfter,
			}, nil
		}
	}
	return Decision{Allowed: true}, nil
}

// Reset clears the window for a key, used when Support clears a user's flag.
func (l *Limiter) Reset(ctx context.Context, key string) error {
	keys := make([]string, 0, len(l.rules))
	for _, rule := range l.rules {
		keys = append(keys, fmt.Sprintf("ratelimit:%s:%s:%s", l.prefix, rule.Name, key))
	}
	if err := l.client.Raw().Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("redisx: reset rate limit for %s: %w", key, err)
	}
	return nil
}

// Count returns the number of events currently inside the named rule's window.
func (l *Limiter) Count(ctx context.Context, key, ruleName string) (int64, error) {
	redisKey := fmt.Sprintf("ratelimit:%s:%s:%s", l.prefix, ruleName, key)
	var window time.Duration
	for _, rule := range l.rules {
		if rule.Name == ruleName {
			window = rule.Window
			break
		}
	}
	if window == 0 {
		return 0, fmt.Errorf("redisx: unknown rate limit rule %q", ruleName)
	}

	cutoff := time.Now().Add(-window).UnixMilli()
	count, err := l.client.Raw().ZCount(ctx, redisKey, fmt.Sprintf("%d", cutoff), "+inf").Result()
	if err != nil {
		return 0, fmt.Errorf("redisx: count rate limit events: %w", err)
	}
	return count, nil
}

func parseDecision(result []any) (allowed bool, count, oldest int64, err error) {
	if len(result) < 3 {
		return false, 0, 0, fmt.Errorf("redisx: unexpected rate limiter reply of length %d", len(result))
	}
	allowedValue, ok := result[0].(int64)
	if !ok {
		return false, 0, 0, fmt.Errorf("redisx: unexpected rate limiter reply type %T", result[0])
	}
	countValue, _ := result[1].(int64)
	oldestValue, _ := result[2].(int64)
	return allowedValue == 1, countValue, oldestValue, nil
}
