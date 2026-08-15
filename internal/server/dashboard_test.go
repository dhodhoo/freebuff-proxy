package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
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

// Five failed logins lock the IP out for a minute, even with the right token.
func TestDashboardLoginRateLimit(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	for range maxLoginFailsForTest {
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

// maxLoginFailsForTest mirrors the server's maxLoginFails constant.
const maxLoginFailsForTest = 5
