package egress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestProxyHealthStateMachine guards the auto-degrade contract (issue #88):
// maxFailures CONSECUTIVE failures mark a proxy degraded (out-of-rotation);
// any success recovers it immediately; unknown addresses are always healthy.
func TestProxyHealthStateMachine(t *testing.T) {
	h := NewProxyHealth(3)
	if _, ok := h.Add("socks5://127.0.0.1:1080"); !ok {
		t.Fatal("Add failed for valid proxy")
	}

	// Unknown address → healthy (dialer never blocked by a registry gap).
	if !h.IsHealthy("127.0.0.1:9999") {
		t.Error("unknown address reported unhealthy")
	}

	const addr = "127.0.0.1:1080"
	for i := 1; i <= 2; i++ {
		h.RecordFailure(addr, errors.New("connection refused"))
		if !h.IsHealthy(addr) {
			t.Fatalf("degraded after %d failures, want healthy until %d", i, 3)
		}
	}
	h.RecordFailure(addr, errors.New("connection refused"))
	if h.IsHealthy(addr) {
		t.Error("still healthy after 3 consecutive failures, want degraded")
	}
	snap := h.Snapshot()
	if len(snap) != 1 || !snap[0].Degraded || snap[0].FailureCount != 3 {
		t.Fatalf("snapshot = %+v, want 1 degraded record with 3 failures", snap)
	}
	if snap[0].LastError != "connection refused" {
		t.Errorf("LastError = %q, want the last failure text", snap[0].LastError)
	}

	// One success recovers immediately (failure counter resets).
	h.RecordSuccess(addr, 42, "203.0.113.9", "DE", "Berlin", "Berlin")
	if !h.IsHealthy(addr) {
		t.Error("not recovered after a success")
	}
	snap = h.Snapshot()
	if len(snap) != 1 || snap[0].Degraded || snap[0].FailureCount != 0 {
		t.Fatalf("snapshot after recovery = %+v, want healthy record", snap)
	}
	if snap[0].LatencyMS != 42 || snap[0].ExitIP != "203.0.113.9" ||
		snap[0].Country != "DE" || snap[0].Region != "Berlin" || snap[0].City != "Berlin" {
		t.Errorf("geo/latency not recorded: %+v", snap[0])
	}
	if snap[0].LastOKAt.IsZero() {
		t.Error("LastOKAt not set on success")
	}

	// maxFailures <= 0 falls back to the default.
	if NewProxyHealth(0).maxFailures != DefaultHealthMaxFailures {
		t.Error("NewProxyHealth(0) did not fall back to the default max failures")
	}
}

// TestProxyHealthDedupe guards dedup by normalized host:port: the same
// endpoint configured with credentials, without, or repeated is tracked once
// (issue #88: "dedupe identical proxy URLs").
func TestProxyHealthDedupe(t *testing.T) {
	h := NewProxyHealth(3)
	variants := []string{
		"socks5://127.0.0.1:1080",
		"socks5://user:pass@127.0.0.1:1080",
		"127.0.0.1:1080",
		"socks5://127.0.0.1:1080",
	}
	for i, raw := range variants {
		addr, added := h.Add(raw)
		if addr != "127.0.0.1:1080" {
			t.Errorf("Add(%q) addr = %q, want normalized 127.0.0.1:1080", raw, addr)
		}
		if i > 0 && added {
			t.Errorf("Add(%q) reported newly-added for a duplicate endpoint", raw)
		}
	}
	snap := h.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot = %d records, want 1 (deduped)", len(snap))
	}
	// The record carries the redacted raw form; a credential must never leak.
	if snap[0].ProxyURL == "" || strings.Contains(snap[0].ProxyURL, "pass") {
		t.Errorf("ProxyURL = %q, want a redacted non-empty URL", snap[0].ProxyURL)
	}

	// Invalid entries are rejected, not tracked.
	if addr, ok := h.Add("socks5://:bad"); ok || addr != "" {
		t.Errorf("Add(invalid) = %q/%v, want rejected", addr, ok)
	}
}

// TestProbeHealthParsing guards the ip-api JSON parse, the fail-status
// branch, and the Cloudflare-trace fallback.
func TestProbeHealthParsing(t *testing.T) {
	t.Run("ip-api success", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","country":"Germany","regionName":"Berlin","city":"Berlin","query":"203.0.113.9"}`))
		}))
		defer ts.Close()
		res, err := probeHealth(context.Background(), nil, ts.URL, 5*time.Second)
		if err != nil {
			t.Fatalf("probeHealth: %v", err)
		}
		if res.IP != "203.0.113.9" || res.Country != "Germany" || res.Region != "Berlin" || res.City != "Berlin" {
			t.Errorf("result = %+v, want 203.0.113.9/Germany/Berlin/Berlin", res)
		}
	})

	t.Run("ip-api fail status surfaces message", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"fail","message":"reserved range"}`))
		}))
		defer ts.Close()
		_, err := probeHealth(context.Background(), nil, ts.URL, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "reserved range") {
			t.Fatalf("err = %v, want the upstream fail message", err)
		}
	})

	t.Run("cloudflare trace fallback", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ip=203.0.113.7\nloc=JP\n"))
		}))
		defer ts.Close()
		res, err := probeHealth(context.Background(), nil, ts.URL, 5*time.Second)
		if err != nil {
			t.Fatalf("probeHealth (trace): %v", err)
		}
		if res.IP != "203.0.113.7" || res.Country != "JP" || res.Region != "" || res.City != "" {
			t.Errorf("result = %+v, want 203.0.113.7/JP with empty region/city", res)
		}
	})

	t.Run("non-200 status is an error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer ts.Close()
		if _, err := probeHealth(context.Background(), nil, ts.URL, 5*time.Second); err == nil {
			t.Fatal("probeHealth succeeded against a 500")
		}
	})

	t.Run("unparseable body is an error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json at all"))
		}))
		defer ts.Close()
		if _, err := probeHealth(context.Background(), nil, ts.URL, 5*time.Second); err == nil {
			t.Fatal("probeHealth succeeded against an opaque body")
		}
	})
}

// TestRunHealthLoop guards the background health loop: it registers +
// probes immediately, records successes through a SOCKS5 proxy, records
// failures for unreachable proxies, re-probes on the interval, and exits on
// ctx cancel without leaking goroutines.
func TestRunHealthLoop(t *testing.T) {
	orig := HealthProbeURL
	defer func() { HealthProbeURL = orig }()

	t.Run("success records exit geo", func(t *testing.T) {
		probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success", "country": "US", "regionName": "CA", "city": "Fremont", "query": "198.51.100.7",
			})
		}))
		defer probe.Close()
		HealthProbeURL = probe.URL

		srv := newSocks5TestServer(t, false, "", "")
		// The health probe target is the httptest server, reached through
		// the socks5 test proxy, which itself dials the target — so the
		// probe succeeds end to end through the proxy.
		h := NewProxyHealth(3)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunHealthLoop(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), h,
				[]string{"socks5://" + srv.Addr()}, 25*time.Millisecond, 5*time.Second, 2)
		}()
		defer func() { cancel(); <-done }()

		poll(t, 2*time.Second, func() bool {
			snap := h.Snapshot()
			return len(snap) == 1 && snap[0].ExitIP == "198.51.100.7" &&
				snap[0].Country == "US" && snap[0].Region == "CA" && snap[0].City == "Fremont" && !snap[0].Degraded
		})

		// A later failure (probe endpoint dies) degrades the proxy.
		probe.Close()
		poll(t, 3*time.Second, func() bool {
			snap := h.Snapshot()
			return len(snap) == 1 && snap[0].Degraded && snap[0].FailureCount >= 3
		})
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunHealthLoop did not exit on ctx cancel")
		}
	})

	t.Run("unreachable proxy records failures", func(t *testing.T) {
		probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","query":"198.51.100.1","country":"US"}`))
		}))
		defer probe.Close()
		HealthProbeURL = probe.URL

		// A SOCKS5 proxy that accepts and then closes the connection: the
		// probe fails, degrading the entry.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close() // hang up mid-handshake
			}
		}()

		h := NewProxyHealth(2)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunHealthLoop(ctx, slog.Default(), h,
				[]string{"socks5://" + ln.Addr().String()}, 15*time.Millisecond, 2*time.Second, 1)
		}()
		defer func() { cancel(); <-done }()

		poll(t, 3*time.Second, func() bool {
			snap := h.Snapshot()
			return len(snap) == 1 && snap[0].Degraded && snap[0].LastError != ""
		})
	})

	t.Run("duplicate proxies deduped", func(t *testing.T) {
		probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","query":"198.51.100.2","country":"US"}`))
		}))
		defer probe.Close()
		HealthProbeURL = probe.URL

		srv := newSocks5TestServer(t, false, "", "")
		h := NewProxyHealth(3)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunHealthLoop(ctx, slog.Default(), h,
				[]string{"socks5://" + srv.Addr(), "socks5://user:pw@" + srv.Addr()}, 10*time.Hour, 5*time.Second, 2)
		}()
		defer func() { cancel(); <-done }()

		poll(t, 2*time.Second, func() bool {
			snap := h.Snapshot()
			return len(snap) == 1 && snap[0].ExitIP == "198.51.100.2"
		})
	})
}

// TestRunHealthLoopBoundedConcurrency guards the bounded probe fan-out: at
// most `concurrency` probes run simultaneously (no unbounded goroutine
// storm). A proxy listener that accepts and stalls the SOCKS5 greeting lets
// the test count in-flight probes by open connections.
func TestRunHealthLoopBoundedConcurrency(t *testing.T) {
	orig := HealthProbeURL
	defer func() { HealthProbeURL = orig }()
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer probe.Close()
	HealthProbeURL = probe.URL

	var inflight, maxInflight atomic.Int64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	// Three distinct proxy endpoints (different ports) that accept and stall
	// the SOCKS5 greeting; the probe URL is never reached.
	stall := func(ln net.Listener) {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			cur := inflight.Add(1)
			for {
				prev := maxInflight.Load()
				if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
					break
				}
			}
			go func(c net.Conn) {
				defer func() { inflight.Add(-1); _ = c.Close() }()
				// Never answer the SOCKS5 greeting; the probe hangs until its
				// timeout (bounded by the health loop, not this goroutine).
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}
	go stall(ln)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln2.Close() }()
	go stall(ln2)
	ln3, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln3.Close() }()
	go stall(ln3)

	// 3 distinct proxy endpoints; concurrency 2 bounds the fan-out.
	h := NewProxyHealth(3)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunHealthLoop(ctx, slog.Default(), h,
			[]string{"socks5://" + ln.Addr().String(), "socks5://" + ln2.Addr().String(), "socks5://" + ln3.Addr().String()},
			5*time.Hour, 150*time.Millisecond, 2)
	}()
	defer func() { cancel(); <-done }()

	// Two proxies in-flight proves the fan-out actually ran.
	poll(t, 2*time.Second, func() bool { return maxInflight.Load() >= 2 })
	// Let several probe rounds elapse; concurrency must never exceed 2.
	time.Sleep(200 * time.Millisecond)
	if m := maxInflight.Load(); m > 2 {
		t.Errorf("max concurrent probes = %d, want <= 2", m)
	}
}
