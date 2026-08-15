package server

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// Internal (package server) auth tests: these exercise adminAuth with the
// real constants, so the lockout bound and map cap cannot drift from the
// public behavior the dashboard_test.go rate-limit test depends on.

func TestAdminAuthLockoutBound(t *testing.T) {
	a := newAdminAuth()
	// maxLoginFails wrong attempts fill the counter; the next allow() must
	// deny while the lockout window is active.
	for range maxLoginFails {
		a.recordFail("10.0.0.1")
	}
	if a.allow("10.0.0.1") {
		t.Fatal("allow() = true after maxLoginFails failures, want locked out")
	}
	// A successful login from the same IP clears the lockout.
	a.clearFails("10.0.0.1")
	if !a.allow("10.0.0.1") {
		t.Fatal("allow() = false after clearFails, want allowed")
	}
}

func TestAdminAuthExpiredLockoutEvicts(t *testing.T) {
	a := newAdminAuth()
	a.fails["10.0.0.9"] = failEntry{count: 0, until: time.Now().Add(-time.Second)}
	if !a.allow("10.0.0.9") {
		t.Fatal("allow() = false after lockout expiry, want allowed")
	}
	if _, ok := a.fails["10.0.0.9"]; ok {
		t.Fatal("expired fail entry not evicted")
	}
}

func TestAdminAuthFailsMapCapped(t *testing.T) {
	a := newAdminAuth()
	// More distinct fresh-lockout IPs than the cap: the map must stay
	// bounded even though no entry has expired.
	for i := range loginFailsCap + 100 {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		a.recordFail(ip)
	}
	if got := len(a.fails); got > loginFailsCap {
		t.Fatalf("fails map = %d entries, want <= %d", got, loginFailsCap)
	}
}

func TestAdminCookieSecureFlag(t *testing.T) {
	a := newAdminAuth()
	rec := httptest.NewRecorder()
	a.setCookie(rec, true)
	c := rec.Result().Cookies()[0]
	if !c.Secure {
		t.Error("cookie Secure flag not set when requested")
	}
	rec = httptest.NewRecorder()
	a.setCookie(rec, false)
	c = rec.Result().Cookies()[0]
	if c.Secure {
		t.Error("cookie Secure flag set for plain-HTTP loopback")
	}
}
