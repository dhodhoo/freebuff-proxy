package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// assertNoTmpFiles fails if writeFileAtomic left its temp file behind.
func assertNoTmpFiles(t *testing.T, dir, base string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// writeFileAtomic must atomically replace an existing file and clean up its
// temp file, on every platform (no unconditional pre-remove on Windows).
func TestWriteFileAtomicReplacesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("NEW\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW\n" {
		t.Errorf("content after write = %q, want %q", got, "NEW\n")
	}
	assertNoTmpFiles(t, dir, ".env")
}

// On failure the target must be left exactly as it was and the temp file
// cleaned up. A non-empty directory cannot be replaced by a rename on any
// platform, so it doubles as a deterministic failure injection.
func TestWriteFileAtomicFailurePreservesTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(kept, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("NEW\n")); err == nil {
		t.Fatal("writeFileAtomic over a non-empty directory succeeded, want error")
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("target dir missing or not a dir after failed write: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("target content lost after failed write: %v", err)
	}
	assertNoTmpFiles(t, dir, ".env")
}
