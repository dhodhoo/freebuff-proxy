package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterDisabled(t *testing.T) {
	l := New(0, 0, 100)
	for range 100 {
		allowed, retryAfter := l.Allow("127.0.0.1:1234")
		if !allowed || retryAfter != 0 {
			t.Fatalf("expected allowed=true, retryAfter=0 when disabled; got allowed=%v, retryAfter=%v", allowed, retryAfter)
		}
	}
	if l.Len() != 0 {
		t.Errorf("expected 0 tracked entries when disabled, got %d", l.Len())
	}
}

func TestLimiterBurstAndRefill(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	l := New(10, 5, 100)
	l.nowFunc = func() time.Time { return now }

	ip := "192.168.1.10:8080"

	// 5 burst requests should all succeed
	for i := range 5 {
		allowed, retryAfter := l.Allow(ip)
		if !allowed {
			t.Fatalf("request %d should be allowed, got retryAfter=%v", i+1, retryAfter)
		}
	}

	// 6th request should fail immediately
	allowed, retryAfter := l.Allow(ip)
	if allowed {
		t.Fatalf("6th request should be rejected (burst 5 exhausted)")
	}
	if retryAfter < 1*time.Second {
		t.Errorf("retryAfter should be >= 1s, got %v", retryAfter)
	}

	// Advance time by 0.5s (should refill 5 tokens at 10 tokens/s)
	now = now.Add(500 * time.Millisecond)

	// 5 requests should now succeed
	for i := range 5 {
		allowed, retryAfter := l.Allow(ip)
		if !allowed {
			t.Fatalf("refilled request %d should be allowed, got retryAfter=%v", i+1, retryAfter)
		}
	}

	// Next request fails again
	allowed, _ = l.Allow(ip)
	if allowed {
		t.Fatalf("request after refilled burst should be rejected")
	}
}

func TestLimiterIPIsolation(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	l := New(1, 1, 100)
	l.nowFunc = func() time.Time { return now }

	ip1 := "10.0.0.1:1000"
	ip2 := "10.0.0.2:2000"

	// Exhaust ip1
	allowed, _ := l.Allow(ip1)
	if !allowed {
		t.Fatal("ip1 first request should be allowed")
	}
	allowed, _ = l.Allow(ip1)
	if allowed {
		t.Fatal("ip1 second request should be rejected")
	}

	// ip2 should still be allowed
	allowed, _ = l.Allow(ip2)
	if !allowed {
		t.Fatal("ip2 first request should be allowed despite ip1 exhaustion")
	}
}

func TestLimiterPortStripping(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	l := New(1, 1, 100)
	l.nowFunc = func() time.Time { return now }

	// First request from port 1111
	allowed, _ := l.Allow("127.0.0.1:1111")
	if !allowed {
		t.Fatal("first request should be allowed")
	}

	// Second request from different port 2222 should hit same bucket and be rejected
	allowed, _ = l.Allow("127.0.0.1:2222")
	if allowed {
		t.Fatal("second request from same IP with different port should be rejected")
	}

	if l.Len() != 1 {
		t.Errorf("expected 1 tracked IP bucket, got %d", l.Len())
	}
}

func TestLimiterMaxEntriesEviction(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	l := New(10, 10, 5)
	l.nowFunc = func() time.Time { return now }

	// Add 5 IPs (at capacity)
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"}
	for _, ip := range ips {
		l.Allow(ip)
	}
	if l.Len() != 5 {
		t.Errorf("expected 5 entries, got %d", l.Len())
	}

	// Adding 6th IP should trigger eviction
	l.Allow("6.6.6.6")
	if l.Len() > 5 {
		t.Errorf("expected <= 5 entries after eviction, got %d", l.Len())
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := New(1000, 500, 1000)
	var wg sync.WaitGroup
	workers := 10
	iterations := 100

	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range iterations {
				l.Allow("10.0.0.1:5000")
				l.Allow("10.0.0.2:5000")
			}
		}(i)
	}
	wg.Wait()
}

func TestLimiterSetRate(t *testing.T) {
	l := New(0, 0, 100)
	r, b := l.Rate()
	if r != 0 || b != 0 {
		t.Errorf("initial rate=%v, burst=%v; want 0, 0", r, b)
	}

	l.SetRate(25, 50)
	r, b = l.Rate()
	if r != 25 || b != 50 {
		t.Errorf("updated rate=%v, burst=%v; want 25, 50", r, b)
	}
}
