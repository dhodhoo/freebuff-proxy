package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
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
