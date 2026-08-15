package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
