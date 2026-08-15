package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// dashboardURL returns the base URL of a test server with AdminToken set.
func dashboardServer(t *testing.T, adminToken string, mut func(*config.Config)) *httptest.Server {
	t.Helper()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = adminToken
		if mut != nil {
			mut(c)
		}
	}, testutil.NewMock())
	return ts
}

// noRedirectClient never follows redirects, so tests can assert on them.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func get(t *testing.T, url, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postLogin(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("token="+token))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The cookie's Secure flag follows the login transport: HTTPS (or a
// TLS-terminating proxy via X-Forwarded-Proto) sets it, plain HTTP does not
// (a Secure cookie over plain HTTP would be rejected and break login).
func TestDashboardCookieSecureFollowsTransport(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	plain := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = plain.Body.Close() }()
	if c := plain.Cookies(); len(c) == 1 && c[0].Secure {
		t.Error("plain-HTTP login set a Secure cookie")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader("token=secret"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	c := resp.Cookies()
	if len(c) != 1 || !c[0].Secure {
		t.Error("X-Forwarded-Proto: https login did not set a Secure cookie")
	}
}

// The dashboard is open when ADMIN_TOKEN is unset (legacy behavior).
func TestDashboardOpenWithoutAdminToken(t *testing.T) {
	ts := dashboardServer(t, "", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (open dashboard)", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Overview") {
		t.Error("dashboard page missing overview heading")
	}
}

// Without a session cookie the dashboard redirects to the login page.
func TestDashboardRedirectsToLogin(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("redirect location = %q, want /admin/login", loc)
	}
}

// Login flow: wrong token rejected, right token issues a cookie that unlocks
// the dashboard, and the cookie is HttpOnly + SameSite=Strict.
func TestDashboardLoginFlow(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	// Wrong token: page re-rendered with an error, no cookie set.
	resp := postLogin(t, ts.URL+"/admin/login", "wrong")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong-token status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Invalid admin token.") {
		t.Error("wrong-token response missing error message")
	}
	if c := resp.Cookies(); len(c) != 0 {
		t.Errorf("wrong token set cookies: %v", c)
	}

	// Correct token: redirect to /admin plus the session cookie.
	resp = postLogin(t, ts.URL+"/admin/login", "secret")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("login redirect = %q, want /admin", loc)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags wrong: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}

	// The cookie unlocks /admin.
	authed := get(t, ts.URL+"/admin", c.Name+"="+c.Value)
	defer func() { _ = authed.Body.Close() }()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("authed status = %d, want 200", authed.StatusCode)
	}
	if !strings.Contains(bodyOf(t, authed), "Overview") {
		t.Error("authed dashboard missing overview heading")
	}
}

// Tampered cookies are rejected (HMAC validation).
func TestDashboardRejectsTamperedCookie(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	// A syntactically valid-looking but unsigned cookie value.
	resp := get(t, ts.URL+"/admin", "fb_admin=9999999999.deadbeef")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("tampered-cookie status = %d, want 302 redirect to login", resp.StatusCode)
	}
}

// lockoutBound matches server.maxLoginFails (5), pinned by
// TestAdminAuthLockoutBound in auth_internal_test.go; this test drives the
// HTTP surface with one more attempt than the bound.
const lockoutBound = 5

// Assets are public (the login page loads them without a cookie).
func TestDashboardAssetsPublic(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin/assets/pico.min.css", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200 without a cookie", resp.StatusCode)
	}
}

// With ADMIN_TOKEN unset, secret-bearing routes (config) require a loopback
// client; a remote client gets 403, not the .env.
func TestDashboardConfigLoopbackGate(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())
	// httptest.NewRequest defaults RemoteAddr to a non-loopback address.
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote config status = %d, want 403", rec.Code)
	}
}

// htmx polls get 401 + HX-Redirect on an expired/missing session, not a 302
// that would swap a login fragment into the dashboard.
func TestDashboardHXAuthRedirect(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("hx status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/admin/login" {
		t.Fatalf("HX-Redirect = %q, want /admin/login", got)
	}
}

func TestDashboardLoginRateLimit(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	for range lockoutBound + 1 {
		resp := postLogin(t, ts.URL+"/admin/login", "wrong")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Too many failed attempts") {
		t.Error("rate-limited login did not show lockout message")
	}
}

// --- config editor ---

// postConfig submits .env content to the authed config endpoint.
func postConfig(t *testing.T, url, cookie, content string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/admin/config", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// authedCookie logs into the test dashboard and returns the session cookie.
func authedCookie(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login issued %d cookies, want 1", len(cookies))
	}
	return cookies[0].Name + "=" + cookies[0].Value
}

// Token actions: unlock/finish/test endpoints work with a session cookie.
func TestDashboardTokenActions(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Unlock a token that has no lock — the action is idempotent success.
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/unlock")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Token 0 unlocked") {
		t.Errorf("unlock response = %q, want success message", body)
	}

	// Out-of-range token fails cleanly.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/9/unlock")
	if !strings.Contains(bodyOf(t, resp), "out of range") {
		t.Error("out-of-range unlock did not report failure")
	}

	// Finish runs: succeeds (mock has no runs to finish).
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/finish")
	if !strings.Contains(bodyOf(t, resp), "runs finished") {
		t.Error("finish action did not report success")
	}

	// Test: real session handshake against the mock upstream.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/test")
	if !strings.Contains(bodyOf(t, resp), "session handshake succeeded") {
		t.Error("test action did not report success")
	}
}

func doTokenAction(t *testing.T, url, cookie, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Runtime token management endpoints: add, remove, mode switch, persisted to
// .env (isolated via t.Chdir).
func TestDashboardTokenAddRemoveMode(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Add a token: pool grows, .env updated.
	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_newtoken123"}`)
	if !strings.Contains(bodyOf(t, resp), "Token added") {
		t.Errorf("add response = %q, want success", bodyOf(t, resp))
	}
	env, _ := os.ReadFile(".env")
	if !strings.Contains(string(env), "cb_newtoken123") {
		t.Error("added token not persisted to .env")
	}

	// Mode switch to bridge: pool empties, AUTH_TOKENS cleared.
	resp = postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"bridge"}`)
	if !strings.Contains(bodyOf(t, resp), "Switched to bridge mode") {
		t.Errorf("mode response = %q, want bridge switch", bodyOf(t, resp))
	}
	env, _ = os.ReadFile(".env")
	if strings.Contains(string(env), "cb_newtoken123") {
		t.Error("token still in .env after bridge switch")
	}
}

// TestDashboardModeSwitchClearsJSONConfigTokens is the regression for the
// reported bug "EXE still shows bridge mode false after clicking": when
// AUTH_TOKENS come from a -config JSON file (common for the Windows binary),
// the mode switch writes AUTH_TOKENS= into .env, and the reload must land in
// bridge mode — the empty .env value has to clear the JSON-provided tokens.
// The pool must also be drained only after the reload verifies bridge mode.
func TestDashboardModeSwitchClearsJSONConfigTokens(t *testing.T) {
	t.Chdir(t.TempDir())

	// A -config JSON supplying the tokens, exactly how the EXE is run.
	if err := os.WriteFile("config.json", []byte(`{"AUTH_TOKENS":["tok-0"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "config.json")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/mode", `{"mode":"bridge"}`)
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Switched to bridge mode") {
		t.Fatalf("mode response = %q, want bridge switch", body)
	}

	// The reload must now be in bridge mode: empty AUTH_TOKENS= in .env
	// beats the JSON list, so BridgeMode() is true on a fresh Load too.
	env, _ := os.ReadFile(".env")
	if strings.Contains(string(env), "tok-0") {
		t.Error("token still in .env after bridge switch")
	}
	reloaded, err := config.Load("config.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.BridgeMode() {
		t.Error("reloaded config not in bridge mode: JSON tokens not cleared by empty .env AUTH_TOKENS")
	}
	if got := p.TokenCount(); got != 0 {
		t.Errorf("pool TokenCount = %d, want 0 after bridge switch", got)
	}
}

func postJSON(t *testing.T, url, cookie, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A valid .env save persists the file and reports success.
func TestDashboardConfigSave(t *testing.T) {
	t.Chdir(t.TempDir())
	// Seed a prior .env with an unrelated key to prove the editor replaces
	// the file wholesale (not merge).
	if err := os.WriteFile(".env", []byte("SAFE_MODE=false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	content := "# my config\nMAX_MESSAGES_PER_DAY=7\nSAFE_MODE=true\n"
	resp := postConfig(t, ts.URL, cookie, content)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Saved and reloaded") {
		t.Error("save response missing success class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".env after save = %q, want %q", got, content)
	}
}

// A rejected save restores the previous .env content (rollback).
func TestDashboardConfigSaveRejectedRollsBack(t *testing.T) {
	t.Chdir(t.TempDir())
	original := "SAFE_MODE=true\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// LISTEN_ADDR without a port fails Validate — the save must be rejected
	// and the file restored.
	resp := postConfig(t, ts.URL, cookie, "LISTEN_ADDR=127.0.0.1\n")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Configuration rejected") {
		t.Error("rejected save missing error class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after rejected save = %q, want original %q", got, original)
	}
}

// The config page renders the effective values with secrets redacted.
func TestDashboardConfigPage(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	resp := get(t, ts.URL+"/admin/config", cookie)
	defer func() { _ = resp.Body.Close() }()
	page := bodyOf(t, resp)
	for _, want := range []string{"Effective configuration", "LISTEN_ADDR", "AUTH_TOKENS", "Editor", "Save &amp; reload"} {
		if !strings.Contains(page, want) {
			t.Errorf("config page missing %q", want)
		}
	}
}
