package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupAiderConfigPreservesUserConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	original := "theme: dracula\neditor: vim\nmodel: gpt-4o\n# my custom comment\nopenai-api-base: http://old.example/v1\nsome-other-setting: true\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"theme: dracula",
		"editor: vim",
		"# my custom comment",
		"some-other-setting: true",
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}

	if strings.Contains(got, "http://old.example/v1") {
		t.Errorf("old openai-api-base value was not replaced:\n%s", got)
	}
	if strings.Contains(got, "gpt-4o") {
		t.Errorf("old model value was not replaced:\n%s", got)
	}
	for _, key := range []string{"openai-api-base:", "openai-api-key:", "model:"} {
		if n := strings.Count(got, key); n != 1 {
			t.Errorf("expected exactly one %q line, got %d:\n%s", key, n, got)
		}
	}
}

func TestSetupAiderConfigAppendsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	original := "theme: dracula\ncustom-setting: keep-me\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"theme: dracula",
		"custom-setting: keep-me",
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "model: openai/deepseek/deepseek-v4-flash") {
		t.Errorf("missing proxy keys should be appended at the end; got:\n%s", got)
	}
}

func TestSetupAiderConfigFreshFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if n := strings.Count(got, "\n"); n != 3 {
		t.Errorf("expected 3 lines for a fresh config, got %d lines:\n%s", n, got)
	}
	for _, want := range []string{
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestSetupAiderConfigShortCircuitsWhenAlreadyConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	original := "theme: dracula\nopenai-api-base: http://localhost:3457/v1\nopenai-api-key: not-needed\nmodel: openai/deepseek/deepseek-v4-flash\ncustom: keep-me\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupAiderConfig(cfgPath) {
		t.Fatal("setupAiderConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("already-configured file was modified:\n%s", string(out))
	}
}

func TestMergeAiderConfigPreservesLineEndings(t *testing.T) {
	original := "theme: dracula\r\nmodel: gpt-4o\r\n"
	lines := []string{
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	}
	got := mergeAiderConfig(original, lines)
	if !strings.Contains(got, "\r\n") {
		t.Errorf("expected CRLF line endings preserved, got:\n%q", got)
	}
	if !strings.Contains(got, "theme: dracula\r\n") {
		t.Errorf("unrelated CRLF line not preserved:\n%q", got)
	}
	if !strings.Contains(got, "model: openai/deepseek/deepseek-v4-flash\r\n") {
		t.Errorf("replaced model line missing or wrong line ending:\n%q", got)
	}
}
