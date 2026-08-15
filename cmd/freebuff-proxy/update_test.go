package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		`set "TEMP_FILE=` + tmp + `"`,
		`set "EXE_FILE=` + exe + `"`,
		`tasklist /FI "PID eq %TARGET_PID%"`,
		`findstr "%TARGET_PID%"`,
		`move /y "%TEMP_FILE%" "%EXE_FILE%"`,
		`del "%~f0"`,
		"timeout /t 1 /nobreak",
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
