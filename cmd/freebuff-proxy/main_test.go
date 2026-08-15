package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
	defer r.Close()
	defer w.Close()

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
