// Package ratelimit provides an in-memory, thread-safe token-bucket rate limiter
// keyed by client IP to protect upstream from bursts, duplicate storms, and spam.
package ratelimit

import (
	"math"
	"net"
	"sync"
	"time"
)

// Limiter implements a thread-safe token bucket rate limiter keyed by client IP.
// A rate <= 0 disables rate limiting (Allow always returns true, 0).
type Limiter struct {
	mu         sync.Mutex
	rate       float64 // tokens added per second
	burst      float64 // maximum tokens in a bucket
	maxEntries int     // maximum tracked IPs before eviction
	buckets    map[string]*bucket
	nowFunc    func() time.Time // time provider for testing
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New creates a new per-IP Limiter.
// rate is the sustained request rate per second per IP (0 disables rate limiting).
// burst is the maximum burst capacity (if <= 0 and rate > 0, defaults to 2*rate, min 1).
// maxEntries is the maximum number of distinct IPs tracked simultaneously (default 10000).
func New(rate float64, burst int, maxEntries int) *Limiter {
	if rate < 0 {
		rate = 0
	}
	b := float64(burst)
	if b <= 0 && rate > 0 {
		b = math.Max(1, rate*2)
	}
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &Limiter{
		rate:       rate,
		burst:      b,
		maxEntries: maxEntries,
		buckets:    make(map[string]*bucket),
		nowFunc:    time.Now,
	}
}

// Allow reports whether a request from ip is permitted.
// If permitted, it returns (true, 0).
// If rejected due to rate limit exhaustion, it returns (false, retryAfter) where
// retryAfter is the duration until at least 1 token is available (minimum 1 second).
func (l *Limiter) Allow(ip string) (bool, time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}

	// Normalize IP (strip port if provided).
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if ip == "" {
		ip = "unknown"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= l.maxEntries {
			l.pruneOldest(now)
		}
		// Initial bucket starts full (burst capacity), minus 1 for current request.
		l.buckets[ip] = &bucket{
			tokens:   l.burst - 1.0,
			lastSeen: now,
		}
		return true, 0
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastSeen).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.lastSeen = now
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true, 0
	}

	// Calculate wait time for at least 1 token.
	missing := 1.0 - b.tokens
	waitSec := missing / l.rate
	if waitSec < 1.0 {
		waitSec = 1.0
	}
	retryAfter := time.Duration(math.Ceil(waitSec)) * time.Second
	return false, retryAfter
}

// SetRate updates the rate and burst dynamically.
func (l *Limiter) SetRate(rate float64, burst int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if rate < 0 {
		rate = 0
	}
	l.rate = rate
	b := float64(burst)
	if b <= 0 && rate > 0 {
		b = math.Max(1, rate*2)
	}
	l.burst = b
}

// Rate returns the configured rate and burst.
func (l *Limiter) Rate() (float64, int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate, int(l.burst)
}

// pruneOldest removes stale buckets (not seen in > 5 minutes) or oldest entries.
func (l *Limiter) pruneOldest(now time.Time) {
	staleThreshold := now.Add(-5 * time.Minute)
	for k, v := range l.buckets {
		if v.lastSeen.Before(staleThreshold) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxEntries {
		toDrop := len(l.buckets) / 2
		dropped := 0
		for k := range l.buckets {
			delete(l.buckets, k)
			dropped++
			if dropped >= toDrop {
				break
			}
		}
	}
}

// Len returns the count of tracked IPs.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
