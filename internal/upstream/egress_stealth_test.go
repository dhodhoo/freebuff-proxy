package upstream

// Tests for the Wave-5 egress/stealth features: stable egress pinning with
// fallback (#98), HTTP/2 upstream wiring (#51), and the passive risk-engine
// feed (#64). Kept in their own file so concurrent work on client_test.go
// does not collide.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/proxy"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/egress"
	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/testutil"
)

// TestStableEgressPinning guards the hash-pin contract (issue #98): with
// STABLE_EGRESS on, a (token, model) pair resolves to ONE deterministic
// proxy index (hash(token+model) % n) — the same across clients and across
// requests, so a session keeps a stable egress IP. STABLE_EGRESS=false
// restores the legacy per-token binding.
func TestStableEgressPinning(t *testing.T) {
	mk := func(stable bool) *config.Config {
		return testConfig("", func(c *config.Config) {
			c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002", "socks5://127.0.0.1:1003"}
			c.StableEgress = stable
		})
	}
	stashIndex := func(c *Client, model string) int {
		t.Helper()
		req, err := c.newRequest(withModel(context.Background(), model), http.MethodGet, "/api/v1/freebuff/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		idx, ok := req.Context().Value(proxyIndexKey{}).(int)
		if !ok {
			t.Fatal("proxy index not stashed in request context")
		}
		return idx
	}

	const model = "deepseek/deepseek-v4-flash"
	c1, err := NewWithIndex("tok-a", 0, mk(true))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := NewWithIndex("tok-a", 0, mk(true))
	if err != nil {
		t.Fatal(err)
	}
	got := []int{stashIndex(c1, model), stashIndex(c2, model), stashIndex(c1, model)}
	if got[0] != got[1] || got[1] != got[2] {
		t.Errorf("stable pinning not deterministic across requests/clients: %v", got)
	}
	want := stableProxyIndex("tok-a", model, 3)
	if got[0] != want {
		t.Errorf("pinned index = %d, want hash index %d", got[0], want)
	}
	// A different model hashes to a different pin (per-session stability,
	// not per-token rigidity).
	other := stashIndex(c1, "z-ai/glm-5.2")
	if other == got[0] && stableProxyIndex("tok-a", "z-ai/glm-5.2", 3) == got[0] {
		// Collision possible with small n; only meaningful when the hashes
		// differ, so assert the SELECTION follows the hash either way.
		t.Log("models hashed to the same index with n=3; skipping cross-model assertion")
	} else if other != stableProxyIndex("tok-a", "z-ai/glm-5.2", 3) {
		t.Errorf("cross-model index = %d, want hash %d", other, stableProxyIndex("tok-a", "z-ai/glm-5.2", 3))
	}

	// Legacy: STABLE_EGRESS=false → tokenIndex % n regardless of model.
	c3, err := NewWithIndex("tok-b", 1, mk(false))
	if err != nil {
		t.Fatal(err)
	}
	if idx := stashIndex(c3, model); idx != 1 {
		t.Errorf("legacy per-token index = %d, want 1 (tokenIndex mod 3)", idx)
	}

	// A model-less request (session poll) reuses the last model's pin.
	pollReq, err := c1.newRequest(context.Background(), http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	pollIdx, _ := pollReq.Context().Value(proxyIndexKey{}).(int)
	if pollIdx != want {
		t.Errorf("model-less request index = %d, want last-model pin %d", pollIdx, want)
	}
}

// pipeDialer is a proxy.Dialer stub whose Dial either fails with err or
// returns one end of an in-memory pipe (a functioning net.Conn).
type pipeDialer struct{ err error }

func (d pipeDialer) Dial(network, addr string) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	c1, _ := net.Pipe()
	return c1, nil
}

// TestStableEgressDialFallback guards the connect-failure fallback (issue
// #98): when the pinned proxy fails to dial, the next proxy in the pool is
// tried (no session kill); proxies the health registry has degraded are
// skipped (#88); when every proxy fails the first error is returned.
func TestStableEgressDialFallback(t *testing.T) {
	client, err := NewWithIndex("tok", 0, testConfig("", func(c *config.Config) {
		c.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002", "socks5://127.0.0.1:1003"}
		c.StableEgress = true
	}))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("falls back to next proxy on failure", func(t *testing.T) {
		client.socksDialers = []proxy.Dialer{
			pipeDialer{err: errors.New("proxy 0 down")},
			pipeDialer{},
			pipeDialer{},
		}
		conn, err := client.dialSocks5(context.Background(), 0, "tcp", "origin.example:443")
		if err != nil {
			t.Fatalf("dialSocks5 with a dead first proxy: %v", err)
		}
		_ = conn.Close()
	})

	t.Run("returns first error when all proxies fail", func(t *testing.T) {
		client.socksDialers = []proxy.Dialer{
			pipeDialer{err: errors.New("refused a")},
			pipeDialer{err: errors.New("refused b")},
			pipeDialer{err: errors.New("refused c")},
		}
		_, err := client.dialSocks5(context.Background(), 0, "tcp", "origin.example:443")
		if err == nil || !strings.Contains(err.Error(), "refused a") {
			t.Fatalf("err = %v, want the first proxy's failure", err)
		}
	})

	t.Run("skips degraded proxies", func(t *testing.T) {
		health := egress.NewProxyHealth(1)
		health.Add("socks5://127.0.0.1:1001")
		health.RecordFailure("127.0.0.1:1001", errors.New("down"))
		client.SetProxyHealth(health)
		client.socksDialers = []proxy.Dialer{
			pipeDialer{err: errors.New("proxy 0 down")},
			pipeDialer{},
			pipeDialer{},
		}
		// start=0 but the degraded proxy must be skipped → success via 1.
		conn, err := client.dialSocks5(context.Background(), 0, "tcp", "origin.example:443")
		if err != nil {
			t.Fatalf("dialSocks5 with a degraded first proxy: %v", err)
		}
		_ = conn.Close()
	})

	t.Run("all degraded reports out-of-rotation", func(t *testing.T) {
		health := egress.NewProxyHealth(1)
		for _, raw := range []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002", "socks5://127.0.0.1:1003"} {
			addr, _ := health.Add(raw)
			health.RecordFailure(addr, errors.New("down"))
		}
		client.SetProxyHealth(health)
		client.socksDialers = []proxy.Dialer{pipeDialer{}, pipeDialer{}, pipeDialer{}}
		_, err := client.dialSocks5(context.Background(), 0, "tcp", "origin.example:443")
		if err == nil || !strings.Contains(err.Error(), "out of rotation") {
			t.Fatalf("err = %v, want the out-of-rotation message", err)
		}
	})
}

// TestHTTP2UpstreamWiring guards the HTTP2_UPSTREAM wiring (issue #51):
// stealth clients register an http2.Transport for the https scheme (dials
// with the same utls dialer advertising the browser ALPN), plain clients
// leave the stdlib h2 default on, and HTTP2_UPSTREAM=false forces h1 on the
// plain path (empty TLSNextProto map — the documented h2 kill switch).
// Per-request rotation (round-robin/random, multi-proxy) disables h2
// because h2 connection reuse would silently defeat rotation.
//
// Registration is asserted behaviorally: with the stealth h2 transport
// registered, an https request is dispatched to it BEFORE any stdlib dial,
// so its dial failure carries the "stealth: tcp dial failed" wrapper; the
// h1 paths fail with a plain stdlib dial error. No external network is
// touched — 127.0.0.1:1 refuses instantly.
func TestHTTP2UpstreamWiring(t *testing.T) {
	roundTripErr := func(c *Client) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:1/", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.http.Transport.RoundTrip(req)
		if err == nil {
			t.Fatal("RoundTrip to a refused port succeeded")
		}
		return err.Error()
	}

	t.Run("plain default off in direct-config tests", func(t *testing.T) {
		// testConfig leaves HTTP2Upstream=false (zero value); production
		// defaults it true via config.Load. The false path must pin h1.
		plain, err := New("tok", testConfig("", nil))
		if err != nil {
			t.Fatal(err)
		}
		tr := plain.http.Transport.(*http.Transport)
		if tr.TLSNextProto == nil || tr.TLSNextProto["h2"] != nil {
			t.Errorf("HTTP2_UPSTREAM=false must disable h2 (empty TLSNextProto map), got %v", tr.TLSNextProto)
		}
		if msg := roundTripErr(plain); strings.Contains(msg, "stealth:") {
			t.Errorf("plain h1 client dial error = %q, want a plain stdlib error", msg)
		}
	})

	t.Run("plain enabled keeps stdlib h2", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) { cfg.HTTP2Upstream = true }))
		if err != nil {
			t.Fatal(err)
		}
		tr := c.http.Transport.(*http.Transport)
		// The stdlib registers h2 lazily on first use; the wiring contract is
		// that we did NOT disable it and that ForceAttemptHTTP2 stays on.
		if tr.TLSNextProto != nil {
			t.Errorf("HTTP2_UPSTREAM=true must leave TLSNextProto nil (stdlib h2 default), got %v", tr.TLSNextProto)
		}
		if !tr.ForceAttemptHTTP2 {
			t.Error("ForceAttemptHTTP2 must stay true for the stdlib h2 path")
		}
		if msg := roundTripErr(c); strings.Contains(msg, "stealth:") {
			t.Errorf("plain h2 client dial error = %q, want a plain stdlib error", msg)
		}
	})

	t.Run("stealth enabled routes https through the h2 transport", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = true
			cfg.TLSFingerprint = "chrome126"
		}))
		if err != nil {
			t.Fatal(err)
		}
		// The dial failure must carry the stealth wrapper: proof the https
		// request was dispatched to the registered http2.Transport (whose
		// DialTLSContext is the utls dialer) rather than the h1 transport.
		msg := roundTripErr(c)
		if !strings.Contains(msg, "stealth: tcp dial failed") {
			t.Errorf("stealth h2 dial error = %q, want the stealth wrapper (https dispatched to the utls dialer)", msg)
		}
	})

	t.Run("stealth disabled keeps h1", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = false
			cfg.TLSFingerprint = "chrome126"
		}))
		if err != nil {
			t.Fatal(err)
		}
		if c.http2Upstream {
			t.Error("HTTP2_UPSTREAM=false must leave http2Upstream false")
		}
		// The h1 path still dials through the stealth DialTLSContext — the
		// wrapper is expected; the point is that no https h2 transport took
		// over the request (that would need HTTP2_UPSTREAM=true).
		if msg := roundTripErr(c); !strings.Contains(msg, "tcp dial failed") {
			t.Errorf("h1 stealth dial error = %q, want the tcp dial wrapper", msg)
		}
	})

	t.Run("rotation disables h2", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = true
			cfg.TLSFingerprint = "chrome126"
			cfg.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
			cfg.ProxyRotation = "round-robin"
		}))
		if err != nil {
			t.Fatal(err)
		}
		if c.http2Upstream {
			t.Error("http2Upstream still true after rotation disable")
		}
	})

	t.Run("stable multi-proxy keeps h2", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = true
			cfg.TLSFingerprint = "chrome126"
			cfg.SOCKS5Proxies = []string{"socks5://127.0.0.1:1001", "socks5://127.0.0.1:1002"}
			cfg.StableEgress = true
		}))
		if err != nil {
			t.Fatal(err)
		}
		if !c.http2Upstream {
			t.Error("stable-egress multi-proxy must keep h2 (pinned proxy reuse is the point)")
		}
		if msg := roundTripErr(c); !strings.Contains(msg, "stealth: tcp dial failed") {
			t.Errorf("stable h2 dial error = %q, want the stealth wrapper", msg)
		}
	})

	t.Run("plain h2 skipped behind http proxy", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = true
			cfg.HTTPProxy = "http://127.0.0.1:9999"
		}))
		if err != nil {
			t.Fatal(err)
		}
		tr := c.http.Transport.(*http.Transport)
		if tr.TLSNextProto == nil || tr.TLSNextProto["h2"] != nil {
			t.Errorf("HTTP_PROXY plain path must keep h1 (no h2 over CONNECT), got %v", tr.TLSNextProto)
		}
	})
}

// TestSessionResponseFeedsRiskEngine guards the passive risk feed (issue
// #64): a session response carrying ipPrivacySignals and ip_capped
// activeUsersForIp/limit lands in the client's risk engine.
func TestSessionResponseFeedsRiskEngine(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ip_capped","model":"deepseek/deepseek-v4-flash","activeUsersForIp":8,"limit":10,"ipPrivacySignals":["proxy"],"retryAfterMs":500}`)
	}

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	engine := stealth.NewRiskEngine()
	client.risk = engine // isolate from the shared DefaultRiskEngine

	if _, err := client.CreateSession(context.Background()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	st := engine.Score()
	if st.Score != 70 { // 40 signal floor + 30 near-cap (8/10)
		t.Errorf("risk Score = %d, want 70", st.Score)
	}
	if st.Level != stealth.RiskHigh {
		t.Errorf("risk Level = %q, want high", st.Level)
	}
	if len(st.Reasons) != 2 {
		t.Errorf("Reasons = %v, want the signal + cap reasons", st.Reasons)
	}

	// A clean response (no signals, no cap pressure) recovers the engine.
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-clean","model":"deepseek/deepseek-v4-flash"}`)
	}
	if _, err := client.CreateSession(context.Background()); err != nil {
		t.Fatalf("clean CreateSession: %v", err)
	}
	// The clean sample carries no signals; the worst retained sample (from
	// the ring) still drives the score — so assert the engine stays within
	// bounds rather than a specific value (the retained-window semantics are
	// tested in the stealth package).
	if st := engine.Score(); st.Score < 0 || st.Score > 100 {
		t.Errorf("risk Score out of bounds after clean sample: %+v", st)
	}
}
