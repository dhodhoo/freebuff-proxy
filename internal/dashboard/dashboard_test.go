package dashboard_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newTestDashboard wires a real (mock-upstream) stack behind the dashboard:
// one pooled token by default, or bridge mode when tokens is 0.
func newTestDashboard(t *testing.T, tokens int) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, tokens),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	clients := make([]*upstream.Client, 0, tokens)
	sessions := make([]*session.Manager, 0, tokens)
	for i := 0; i < tokens; i++ {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil)
	ts := httptest.NewServer(d.Page("overview"))
	t.Cleanup(ts.Close)
	return ts
}

func TestPageOverviewFull(t *testing.T) {
	ts := newTestDashboard(t, 0) // bridge mode: no fixed tokens
	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	page := string(body[:n])
	for _, want := range []string{"<html", "Overview", "Bridge mode", "models"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestPageOverviewFragment(t *testing.T) {
	ts := newTestDashboard(t, 1) // pooled mode: one fixed token
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	frag := string(body[:n])
	if strings.Contains(frag, "<html") {
		t.Error("htmx request rendered a full page, want bare fragment")
	}
	for _, want := range []string{"Token 0", "low", "session"} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
}

func TestLoginPageRendersError(t *testing.T) {
	cfg := &config.Config{
		UpstreamBaseURL: "https://www.codebuff.com",
		AuthTokens:      []string{},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil)
	rec := httptest.NewRecorder()
	d.RenderLogin(rec, httptest.NewRequest(http.MethodGet, "/admin/login", nil), "Invalid admin token.")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"Sign in", "Invalid admin token."} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("login page missing %q", want)
		}
	}
}
