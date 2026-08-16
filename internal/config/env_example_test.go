package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnvExampleLoadsCleanly proves the shipped .env.example is a valid,
// loadable configuration (a fresh user copying it to .env starts without
// errors) and that every documented safety default lands as expected.
func TestEnvExampleLoadsCleanly(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(.env.example) failed: %v", err)
	}
	if !cfg.SafeMode {
		t.Error("SafeMode = false, want true (anti-ban default)")
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (402 avoidance)", cfg.CostMode)
	}
	if cfg.ListenAddr != "127.0.0.1:3457" {
		t.Errorf("ListenAddr = %q, want loopback default", cfg.ListenAddr)
	}
	if cfg.TLSFingerprint != "auto" {
		t.Errorf("TLSFingerprint = %q, want auto", cfg.TLSFingerprint)
	}
	if cfg.TransientRetries != 1 {
		t.Errorf("TransientRetries = %d, want 1", cfg.TransientRetries)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true (empty AUTH_TOKENS)")
	}
}
