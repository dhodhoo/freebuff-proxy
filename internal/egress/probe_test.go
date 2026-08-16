package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// traceServer serves a Cloudflare-trace-style body at any path so the
// probe can be pointed at it via ProbeURL.
func traceServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestProbeParsesTrace guards the core contract: a successful GET parses
// the ip= and loc= lines into the result, and the request goes through the
// given dialer.
func TestProbeParsesTrace(t *testing.T) {
	const body = "fl=7f0\nh=www.cloudflare.com\nip=203.0.113.7\nloc=JP\ntls=ON\n"
	ts := traceServer(t, body, http.StatusOK)

	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	var dialed atomic.Int64
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Add(1)
		return DirectDialer(5*time.Second)(ctx, network, addr)
	}

	res := Probe(context.Background(), dialer, 5*time.Second)
	if res.Err != nil {
		t.Fatalf("Probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "JP" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / JP", res.IP, res.Country)
	}
	if dialed.Load() == 0 {
		t.Error("probe did not dial through the provided dialer")
	}
}

// TestProbeMissingTraceFields guards fail-open parsing: a 200 body without
// loc (or without either field) must not be an error; missing fields stay
// empty so the caller can report "unavailable".
func TestProbeMissingTraceFields(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.7\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
	if res.Err != nil {
		t.Fatalf("Probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / empty", res.IP, res.Country)
	}
}

// TestProbeErrorStatus guards non-200 handling: a trace endpoint that
// answers with an error status is a probe failure, not a parsed result.
func TestProbeErrorStatus(t *testing.T) {
	ts := traceServer(t, "ip=203.0.113.7\nloc=JP\n", http.StatusInternalServerError)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	res := Probe(context.Background(), DirectDialer(5*time.Second), 5*time.Second)
	if res.Err == nil {
		t.Fatal("Probe succeeded against a 500 response")
	}
	if !strings.Contains(res.Err.Error(), "500") {
		t.Errorf("error %q does not mention the status", res.Err)
	}
}

// TestProbeDialError guards fail-open on an unreachable path: the dialer
// error is returned as Err rather than hanging or panicking.
func TestProbeDialError(t *testing.T) {
	ts := traceServer(t, "ip=1.2.3.4\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	boom := errors.New("dial refused")
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, boom
	}
	res := Probe(context.Background(), dialer, 5*time.Second)
	if !errors.Is(res.Err, boom) {
		t.Errorf("Err = %v, want the dialer error", res.Err)
	}
}

// TestProbeTimeout guards the timeout bound: a dialer that never answers
// must yield an Err within the probe timeout instead of blocking forever.
func TestProbeTimeout(t *testing.T) {
	ts := traceServer(t, "ip=1.2.3.4\nloc=US\n", http.StatusOK)
	orig := ProbeURL
	ProbeURL = ts.URL
	defer func() { ProbeURL = orig }()

	// Dialer that ignores ctx and never completes; the client timeout is
	// what must bound the probe.
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {}
	}
	start := time.Now()
	res := Probe(context.Background(), dialer, 50*time.Millisecond)
	if res.Err == nil {
		t.Fatal("blocked dial did not time out")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probe took %v, want bounded by the 50ms timeout", elapsed)
	}
}

// TestSocks5Dialer guards the SOCKS5 dialer builder: both a bare host:port
// and a socks5:// URL normalize to a usable dialer, and invalid input is
// rejected instead of returning a broken dialer.
func TestSocks5Dialer(t *testing.T) {
	for _, raw := range []string{"127.0.0.1:9050", "socks5://127.0.0.1:9050", "socks5://user:pass@127.0.0.1:9050"} {
		d, err := Socks5Dialer(raw)
		if err != nil {
			t.Errorf("Socks5Dialer(%q): %v", raw, err)
			continue
		}
		if d == nil {
			t.Errorf("Socks5Dialer(%q) returned a nil dialer", raw)
		}
	}
	for _, bad := range []string{"", "socks5://", "://nope"} {
		if _, err := Socks5Dialer(bad); err == nil {
			t.Errorf("Socks5Dialer(%q) succeeded, want error", bad)
		}
	}
}
