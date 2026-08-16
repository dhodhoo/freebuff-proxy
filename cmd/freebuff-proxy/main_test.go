package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"freebuff-proxy/internal/egress"
)

// TestHoldForExitIfConsolePipedStderrNoHang guards the console hold: with
// piped stderr (containers, log files, Task Scheduler, CI) holdForExitIfConsole
// must return immediately — a hang here would freeze every non-interactive
// startup error path.
func TestHoldForExitIfConsolePipedStderrNoHang(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	go func() {
		holdForExitIfConsole()
		close(done)
	}()
	select {
	case <-done:
		// Returned without waiting for input: an anonymous pipe is not a
		// character device, so the hold must be a no-op.
	case <-time.After(2 * time.Second):
		t.Fatal("holdForExitIfConsole blocked on piped stderr")
	}
}

// TestWindowsUpdateScriptASCIIOnlyWithRetry guards the .bat helper template:
// cmd reads batch files with the console codepage, so the whole script must
// be ASCII (paths enter only as %~dp0 + ASCII basenames), and the swap must
// retry the move so a brief AV/Defender lock does not fail the update.
func TestWindowsUpdateScriptASCIIOnlyWithRetry(t *testing.T) {
	// Non-ASCII path (CJK user directory): embedding it verbatim would be
	// mangled by cmd depending on the console codepage.
	exe := `C:\Users\张三\freebuff-proxy.exe`
	tmp := `C:\Users\张三\freebuff-proxy.exe.tmp-123`
	script := windowsUpdateScript(exe, tmp, 4242)

	for _, want := range []string{
		`set "TARGET_PID=4242"`,
		"%~dp0",
		`:retry`,
		`set /a tries=0`,
		`move /y "%TEMP_FILE%" "%EXE_FILE%"`,
		"timeout /t 2 /nobreak",
		`echo OK>`,
		"endlocal",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("helper script missing %q; script:\n%s", want, script)
		}
	}
	// The whole .bat must be ASCII: no raw non-ASCII path bytes.
	for _, r := range script {
		if r > 127 {
			t.Errorf("helper script contains non-ASCII rune %q; script:\n%s", r, script)
		}
	}
	if strings.Contains(script, "张三") {
		t.Error("helper script embeds the raw non-ASCII path")
	}
	if !strings.Contains(script, filepath.Base(exe)) || !strings.Contains(script, filepath.Base(tmp)) {
		t.Error("helper script does not carry the ASCII file basenames")
	}
	// Sprintf escaping must collapse %% -> % (batch variable references).
	if strings.Contains(script, "%%") {
		t.Errorf("helper script contains unescaped %% (Sprintf escaping not applied); script:\n%s", script)
	}
}

// TestShutdownSignals guards the graceful-drain notify set: os.Interrupt and
// SIGTERM must always be registered. Go has no syscall.SIGBREAK constant on
// any platform — on Windows the runtime delivers both Ctrl+C and Ctrl+Break
// as os.Interrupt (runtime/os_windows.go ctrlHandler) — so the Ctrl+Break
// drain behavior itself is pinned by TestCtrlBreakDrainsGracefully
// (main_windows_test.go).
func TestShutdownSignals(t *testing.T) {
	got := shutdownSignals()
	has := func(want os.Signal) bool {
		for _, s := range got {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(os.Interrupt) {
		t.Error("shutdownSignals missing os.Interrupt (covers Ctrl+C and Ctrl+Break on Windows)")
	}
	if !has(syscall.SIGTERM) {
		t.Error("shutdownSignals missing syscall.SIGTERM")
	}
}

// TestEgressCacheGetSet guards the per-egress result cache: Set stores the
// latest result, Get returns it, keys stay independent, and missing keys
// report absent.
func TestEgressCacheGetSet(t *testing.T) {
	c := egress.NewCacheWithTTL(time.Minute)
	if _, ok := c.Get("direct"); ok {
		t.Fatal("empty cache returned a result for direct")
	}
	want := egress.Result{IP: "1.2.3.4", Country: "US"}
	c.Set("direct", want)
	got, ok := c.Get("direct")
	if !ok {
		t.Fatal("cached direct result not found")
	}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	// Overwrite replaces the previous result.
	latest := egress.Result{IP: "5.6.7.8", Country: "DE"}
	c.Set("direct", latest)
	if got, _ := c.Get("direct"); got != latest {
		t.Errorf("Get after overwrite = %+v, want %+v", got, latest)
	}
	// Keys are independent.
	if _, ok := c.Get("proxy-0"); ok {
		t.Error("proxy-0 returned a result that was never set")
	}
}

// TestEgressCacheTTL guards the freshness window: an entry must not be
// returned after its TTL elapses, and a re-Set refreshes the timestamp.
func TestEgressCacheTTL(t *testing.T) {
	c := egress.NewCacheWithTTL(50 * time.Millisecond)
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	if _, ok := c.Get("direct"); !ok {
		t.Fatal("fresh entry not returned")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("direct"); ok {
		t.Error("expired entry still returned")
	}
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	if _, ok := c.Get("direct"); !ok {
		t.Error("re-Set entry not returned")
	}
}
