package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallUnixAtomicSwap(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy")
	newPath := filepath.Join(dir, "freebuff-proxy.new")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := installUnix(execPath, newPath); err != nil {
		t.Fatalf("installUnix: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("installed content = %q, want %q", got, "new-binary")
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("temp file %q should have been consumed by the swap", newPath)
	}
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old backup %q should have been removed", execPath+".old")
	}
}

func TestInstallUnixFailsWhenOldBinaryGone(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy")
	newPath := filepath.Join(dir, "freebuff-proxy.new")
	if err := os.WriteFile(newPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := installUnix(execPath, newPath); err == nil {
		t.Fatal("expected error when current binary is missing")
	}
	// The temp file must survive a failed swap.
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("temp file should survive a failed swap: %v", err)
	}
}

func TestWindowsUpdateScript(t *testing.T) {
	exe := `C:\tools\freebuff proxy\freebuff-proxy.exe`
	tmp := `C:\tools\freebuff proxy\freebuff-proxy.exe.tmp-123`
	script := windowsUpdateScript(exe, tmp, 4242)

	for _, want := range []string{
		`set "TARGET_PID=4242"`,
		`tasklist /FI "PID eq %TARGET_PID%"`,
		`findstr "%TARGET_PID%"`,
		`set "TEMP_FILE=%~dp0`,
		`set "EXE_FILE=%~dp0`,
		`move /y "%TEMP_FILE%" "%EXE_FILE%"`,
		`echo OK>`,
		"timeout /t 1 /nobreak",
		"endlocal",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("helper script missing %q; script:\n%s", want, script)
		}
	}
	// Sprintf escaping must collapse %% -> % (batch variable references).
	if strings.Contains(script, "%%") {
		t.Errorf("helper script contains unescaped %% (Sprintf escaping not applied); script:\n%s", script)
	}
}

// TestWindowsUpdateScriptRunsAndSwaps is the end-to-end Windows check: the
// generated .bat must resolve %~dp0 paths, swap the temp binary over the
// executable, and write an OK result marker. (The helper does not self-delete
// — that races cmd's file reads — so the marker is the source of truth.)
func TestWindowsUpdateScriptRunsAndSwaps(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only deferred swap")
	}
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy.exe")
	tmpPath := filepath.Join(dir, "freebuff-proxy.exe.tmp-123")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	// A pid that is already gone: the helper's waitloop exits immediately
	// instead of waiting for a real process.
	dead := exec.Command("cmd", "/c", "exit")
	if err := dead.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	deadPid := dead.Process.Pid
	_ = dead.Wait()

	batPath := execPath + ".update.bat"
	if err := os.WriteFile(batPath, []byte(windowsUpdateScript(execPath, tmpPath, deadPid)), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("cmd", "/c", batPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper script failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("installed content = %q, want %q", got, "new-binary")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file should be consumed by the swap, stat err = %v", err)
	}
	marker, err := os.ReadFile(execPath + ".update.result")
	if err != nil {
		t.Fatalf("read result marker: %v", err)
	}
	if !strings.HasPrefix(string(marker), "OK") {
		t.Errorf("result marker = %q, want OK prefix", marker)
	}
}

// TestWindowsUpdateScriptDefersSwapUntilParentExits pins the deferred-swap
// contract: the helper launched detached via `cmd /c start /b` must outlive
// the launcher, wait for the updating process (pid) to exit, and only then
// swap the binary and write the OK marker.
func TestWindowsUpdateScriptDefersSwapUntilParentExits(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only deferred swap")
	}
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy.exe")
	tmpPath := filepath.Join(dir, "freebuff-proxy.exe.tmp-123")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	// The "updating parent": a helper process that stays alive ~2s. The swap
	// must NOT happen until it exits.
	parent := exec.Command(os.Args[0], "-test.run=TestWindowsUpdateSleepHelperProcess")
	parent.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := parent.Start(); err != nil {
		t.Fatalf("cannot start updating-parent helper: %v", err)
	}
	parentPid := parent.Process.Pid
	parentDone := make(chan struct{})
	go func() {
		_ = parent.Wait()
		close(parentDone)
	}()
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		<-parentDone
	})

	batPath := execPath + ".update.bat"
	if err := os.WriteFile(batPath, []byte(windowsUpdateScript(execPath, tmpPath, parentPid)), 0755); err != nil {
		t.Fatal(err)
	}

	// Launch exactly like installWindows: detached via cmd /c start /b.
	// Note: use Start+Wait on the launcher, NOT CombinedOutput — the /b
	// helper inherits the launcher's stdio, so CombinedOutput would block
	// until the helper itself finishes, masking the deferred timing.
	start := time.Now()
	launcher := exec.Command("cmd", "/c", "start", "/b", "", batPath)
	if err := launcher.Start(); err != nil {
		t.Fatalf("launch helper script: %v", err)
	}
	if err := launcher.Wait(); err != nil {
		t.Fatalf("launch helper script: %v", err)
	}

	// The swap must be deferred: shortly after launch the parent is still
	// alive, so the old binary must still be in place.
	time.Sleep(500 * time.Millisecond)
	if got, _ := os.ReadFile(execPath); string(got) != "old-binary" {
		t.Fatalf("swap happened before the updating parent exited (content %q)", got)
	}

	// Once the parent exits (~2s), the helper must complete the swap.
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := os.ReadFile(execPath)
		if err == nil && string(got) == "new-binary" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("swap did not happen after the updating parent exited")
		}
		time.Sleep(100 * time.Millisecond)
	}
	marker, err := os.ReadFile(execPath + ".update.result")
	if err != nil {
		t.Fatalf("read result marker: %v", err)
	}
	if !strings.HasPrefix(string(marker), "OK") {
		t.Errorf("result marker = %q, want OK prefix", marker)
	}
	t.Logf("deferred swap completed %v after launch", time.Since(start).Round(100*time.Millisecond))
}

// TestWindowsUpdateSleepHelperProcess is the re-exec helper for
// TestWindowsUpdateScriptDefersSwapUntilParentExits: it simply stays alive
// ~2s so the update helper has something to wait for.
func TestWindowsUpdateSleepHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(2 * time.Second)
}

// TestVerifyChecksumFetchFailureAborts guards the supply-chain guarantee
// (see .github/SECURITY.md): a checksums.txt fetch failure must abort the
// update, not silently proceed unverified.
func TestVerifyChecksumFetchFailureAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := verifyChecksum(context.Background(), &http.Client{Timeout: 5 * time.Second}, srv.URL+"/checksums.txt", []byte("asset-bytes"))
	if err == nil {
		t.Fatal("verifyChecksum succeeded, want error when checksums.txt fetch fails")
	}
	if !strings.Contains(err.Error(), "checksums.txt") {
		t.Errorf("verifyChecksum error = %v, want mention of checksums.txt", err)
	}
}

func TestVerifyChecksumMatchAndMismatch(t *testing.T) {
	assetBytes := []byte("asset-bytes")
	sum := sha256.Sum256(assetBytes)
	checksums := hex.EncodeToString(sum[:]) + "  freebuff-proxy_linux_amd64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	url := srv.URL + "/checksums.txt"

	if err := verifyChecksum(context.Background(), client, url, assetBytes); err != nil {
		t.Fatalf("verifyChecksum(matching) = %v, want nil", err)
	}
	if err := verifyChecksum(context.Background(), client, url, []byte("other-bytes")); err == nil {
		t.Fatal("verifyChecksum(mismatch) = nil, want checksum mismatch error")
	}
}

// TestReportUpdateResultMarkerReportsAndDeletes pins the deferred-swap marker
// contract: a stale <exe>.update.result left by a previous Windows swap is
// surfaced ("Previous deferred update result: ...") and deleted, so a FAILED
// swap is reported exactly once on the next -update invocation.
func TestReportUpdateResultMarkerReportsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "freebuff-proxy.exe")
	marker := updateResultMarker(exe)

	// FAILED case: the previous deferred swap did not complete.
	failed := "FAILED: could not replace the running binary after 5 attempts.\nInstall manually: move \"new\" over \"old\".\n"
	if err := os.WriteFile(marker, []byte(failed), 0644); err != nil {
		t.Fatal(err)
	}
	if got := reportUpdateResultMarker(exe); got != strings.TrimSpace(failed) {
		t.Errorf("reportUpdateResultMarker(FAILED) = %q, want %q", got, strings.TrimSpace(failed))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("FAILED marker was not deleted after reporting")
	}

	// OK case: the previous deferred swap succeeded.
	if err := os.WriteFile(marker, []byte("OK\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := reportUpdateResultMarker(exe); got != "OK" {
		t.Errorf("reportUpdateResultMarker(OK) = %q, want %q", got, "OK")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("OK marker was not deleted after reporting")
	}

	// No marker: no-op, returns "".
	if got := reportUpdateResultMarker(exe); got != "" {
		t.Errorf("reportUpdateResultMarker(no marker) = %q, want \"\"", got)
	}
}
