package dashboard_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/logring"
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
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
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
	page := string(mustReadAll(t, resp))
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
	frag := string(mustReadAll(t, resp))
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
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
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

// newDashboardForPages builds a dashboard with a wired log ring, mounting the
// given page (defaults to "logs").
func newDashboardForPages(t *testing.T, withRing bool, page ...string) *httptest.Server {
	t.Helper()
	name := "logs"
	if len(page) > 0 && page[0] != "" {
		name = page[0]
	}
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}
	var ring *logring.Handler
	if withRing {
		ring = logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
		slog.New(ring).Info("hello ring", "k", "v")
	}
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, ring)
	ts := httptest.NewServer(d.Page(name))
	t.Cleanup(ts.Close)
	return ts
}

func TestLogsPageWithRing(t *testing.T) {
	ts := newDashboardForPages(t, true)
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"hello ring", "ring enabled", "INFO", "k=v"} {
		if !strings.Contains(page, want) {
			t.Errorf("logs page missing %q", want)
		}
	}
}

func TestLogsPageWithoutRing(t *testing.T) {
	ts := newDashboardForPages(t, false)
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(string(mustReadAll(t, resp)), "ring disabled") {
		t.Error("logs page should report the ring as disabled")
	}
}

func TestMetricsPageRendersSparklines(t *testing.T) {
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
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.Page("metrics")(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics", nil))
	page := rec.Body.String()
	for _, want := range []string{"Requests served", "Transient retries", "Fingerprint rotations", "<svg"} {
		if !strings.Contains(page, want) {
			t.Errorf("metrics page missing %q", want)
		}
	}
}

func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The restricted page renders the themed gate (not a bare error line).
func TestRestrictedPageRenders(t *testing.T) {
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
	d := dashboard.New(func() *config.Config { return cfg }, p, reg, nil, nil)
	rec := httptest.NewRecorder()
	d.RenderRestricted(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil), "blocked")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	for _, want := range []string{"Restricted", "ADMIN_TOKEN", "blocked"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("restricted page missing %q", want)
		}
	}
}

// The models page renders the catalog with agent mappings.
func TestModelsPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, false, "models")
	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"Models", "upstream agent", "z-ai/glm-5.2"} {
		if !strings.Contains(page, want) {
			t.Errorf("models page missing %q", want)
		}
	}
}

// The setup page renders the base URL and client snippets.
func TestSetupPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, false, "setup")
	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	for _, want := range []string{"Client setup", "OpenCode", "Continue", "aider", "127.0.0.1", "/v1"} {
		if !strings.Contains(page, want) {
			t.Errorf("setup page missing %q", want)
		}
	}
}

// The traces page renders the recorded chat-trace entry (ring holds a
// non-trace "hello ring" record; the page must not crash and shows the
// empty state when no traces exist).
func TestTracesPageRenders(t *testing.T) {
	ts := newDashboardForPages(t, true, "traces")
	resp, err := http.Get(ts.URL + "/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page := string(mustReadAll(t, resp))
	if !strings.Contains(page, "No chat traffic yet") {
		t.Error("traces page missing empty state")
	}
}
