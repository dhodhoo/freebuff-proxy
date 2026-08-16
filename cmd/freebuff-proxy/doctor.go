package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/egress"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/upstream"
)

// egressRegionRow renders the doctor's egress region line from the direct
// probe cache entry: "Egress region: <country> (<ip>)" on success, an
// "(unavailable)" warning when the probe failed or no result is cached.
func egressRegionRow(cache *egress.Cache) (line string, warn bool) {
	r, ok := cache.Get("direct")
	if !ok || r.Err != nil || r.Country == "" || r.IP == "" {
		reason := "no direct probe result"
		if ok && r.Err != nil {
			reason = fmt.Sprintf("direct probe failed: %v", r.Err)
		}
		return fmt.Sprintf("Egress region: unavailable (%s)", reason), true
	}
	return fmt.Sprintf("Egress region: %s (%s)", r.Country, r.IP), false
}

// runTokenTest probes the first configured token with a real session
// handshake (the same path the pool uses) and exits 0 on success, 1 on
// failure. Exposed as -test-token for installers and scripts.
// probeModel returns the safest model to probe a token with: the fallback
// default (deepseek-v4-flash — the model every account gets, incl. limited
// tier) when in the catalog, else the first catalog model. The alphabetical
// first model (anthropic/claude-fable-5) is a capacity-gated offer model
// that makes token tests fail on most accounts.
func probeModel(reg *registry.Registry) string {
	models := reg.Models()
	if len(models) == 0 {
		return ""
	}
	for _, id := range models {
		if id == "deepseek/deepseek-v4-flash" {
			return id
		}
	}
	return models[0]
}

func runTokenTest(configPath string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: config load failed: %v\n", err)
		os.Exit(1)
	}
	if cfg.BridgeMode() {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: -test-token: no AUTH_TOKENS configured (bridge mode); nothing to probe")
		os.Exit(1)
	}
	reg := registry.New(&cfg, &http.Client{Timeout: 10 * time.Second})
	reg.LoadFallback()
	model := probeModel(reg)
	if model == "" {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: -test-token: registry has no models to probe against")
		os.Exit(1)
	}
	clientCfg := cfg
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	st, err := client.CreateSessionForModel(ctx, model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: token rejected upstream: %v\n", err)
		os.Exit(1)
	}
	endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = client.EndSession(endCtx, st.InstanceID)
	endCancel()
	fmt.Printf("freebuff-proxy: token OK (%s, session %s)\n", model, st.InstanceID)
	os.Exit(0)
}

func runDoctor(configPath string) {
	fmt.Println("freebuff-proxy doctor diagnostic tool")
	fmt.Println("=====================================")

	passed := 0
	warnings := 0
	failed := 0

	ok := func(msg string) {
		fmt.Printf("[ok] %s\n", msg)
		passed++
	}
	warn := func(msg string) {
		fmt.Printf("[!!] %s\n", msg)
		warnings++
	}
	fail := func(msg string) {
		fmt.Printf("[FAIL] %s\n", msg)
		failed++
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fail(fmt.Sprintf("Config loading failed: %v", err))
		fmt.Printf("\nSummary: %d passed, %d warnings, %d failed\n", passed, warnings, failed)
		os.Exit(1)
	}
	ok("Configuration loaded & validated successfully")

	if cfg.BridgeMode() {
		warn("AUTH_TOKENS is empty (bridge mode active). Clients must supply Authorization: Bearer <token>")
	} else {
		ok(fmt.Sprintf("AUTH_TOKENS: %d token(s) configured", len(cfg.AuthTokens)))
		for i, tok := range cfg.AuthTokens {
			if strings.HasPrefix(strings.ToLower(tok), "bearer ") {
				warn(fmt.Sprintf("Token #%d starts with 'Bearer ' prefix -- remove it from .env", i+1))
			} else if tok == "cb_xxx" || tok == "cb_yyy" {
				warn(fmt.Sprintf("Token #%d is a placeholder string %q", i+1, tok))
			} else {
				ok(fmt.Sprintf("Token #%d format valid (%d chars)", i+1, len(tok)))
			}
		}
	}

	// Port availability check
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fail(fmt.Sprintf("Listen address %s is not available: %v", cfg.ListenAddr, err))
	} else {
		_ = ln.Close()
		ok(fmt.Sprintf("Listen address %s is available", cfg.ListenAddr))
	}

	// DNS & TLS reachability check
	targetHost := "www.codebuff.com"
	if u, err := url.Parse(cfg.UpstreamBaseURL); err == nil && u.Host != "" {
		targetHost = u.Host
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, targetHost)
	if err != nil {
		fail(fmt.Sprintf("DNS lookup for %s failed: %v", targetHost, err))
	} else {
		ok(fmt.Sprintf("DNS lookup for %s resolved (%s)", targetHost, strings.Join(addrs, ", ")))
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tlsConn, err := tls.DialWithDialer(dialer, "tcp", targetHost+":443", &tls.Config{ServerName: targetHost})
	if err != nil {
		fail(fmt.Sprintf("TLS connection to %s:443 failed: %v", targetHost, err))
	} else {
		_ = tlsConn.Close()
		ok(fmt.Sprintf("TLS connection to %s:443 succeeded", targetHost))
	}

	// Egress region check: one live probe of the direct outbound path
	// through a plain dialer, read back from the cache the doctor shares
	// with the runtime. A failed probe is a warning, not a doctor failure —
	// the proxy keeps working, only the region readout is missing.
	egressCache := egress.NewCache()
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	res := egress.Probe(probeCtx, egress.DirectDialer(5*time.Second), 5*time.Second)
	probeCancel()
	egressCache.Set("direct", res)
	if line, isWarn := egressRegionRow(egressCache); isWarn {
		warn(line)
	} else {
		ok(line)
	}

	// Registry test
	reg := registry.New(&cfg, &http.Client{Timeout: 10 * time.Second})
	reg.LoadFallback()
	ok(fmt.Sprintf("Model registry offline fallback loaded (%d models, %d agents)", reg.ModelCount(), len(reg.AgentIDs())))

	if err := reg.Refresh(ctx); err != nil {
		warn(fmt.Sprintf("Registry live refresh warning: %v (offline fallback retained)", err))
	} else {
		ok(fmt.Sprintf("Registry live refresh succeeded (%d models)", reg.ModelCount()))
	}

	// Token validity probe: one real session handshake per configured token,
	// through the same client path the pool uses. This is the check that
	// catches expired/revoked tokens before the first chat 401s.
	if !cfg.BridgeMode() {
		probe := probeModel(reg)
		if probe == "" {
			warn("Cannot probe tokens: registry has no models")
		} else {
			for i, tok := range cfg.AuthTokens {
				clientCfg := cfg
				client, err := upstream.New(tok, &clientCfg)
				if err != nil {
					fail(fmt.Sprintf("Token #%d: cannot build client: %v", i+1, err))
					continue
				}
				probeCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				st, err := client.CreateSessionForModel(probeCtx, probe)
				cancel()
				if err != nil {
					fail(fmt.Sprintf("Token #%d validity probe failed: %v (re-run the upstream CLI to refresh the token)", i+1, err))
					continue
				}
				endCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = client.EndSession(endCtx, st.InstanceID)
				cancel()
				ok(fmt.Sprintf("Token #%d validity probe succeeded (session handshake)", i+1))
			}
		}
	}

	fmt.Printf("\nSummary: %d passed, %d warnings, %d failed\n", passed, warnings, failed)
	if failed > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}
