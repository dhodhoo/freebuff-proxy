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
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/upstream"
)

// runTokenTest probes the first configured token with a real session
// handshake (the same path the pool uses) and exits 0 on success, 1 on
// failure. Exposed as -test-token for installers and scripts.
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
	models := reg.Models()
	if len(models) == 0 {
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
	st, err := client.CreateSessionForModel(ctx, models[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -test-token: token rejected upstream: %v\n", err)
		os.Exit(1)
	}
	endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = client.EndSession(endCtx, st.InstanceID)
	endCancel()
	fmt.Printf("freebuff-proxy: token OK (%s, session %s)\n", models[0], st.InstanceID)
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
		models := reg.Models()
		if len(models) == 0 {
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
				st, err := client.CreateSessionForModel(probeCtx, models[0])
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
