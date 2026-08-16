package main

import (
	"encoding/json"
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

func TestSetupContinueYamlConfigMergesIntoExistingModels(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	original := "name: My Workspace\nversion: 1.0\nmodels:\n  - title: \"Existing Model\"\n    provider: \"openai\"\n    model: \"gpt-4o\"\n    apiBase: \"http://existing.example/v1\"\n    apiKey: \"secret\"\n# trailing comment\nagents:\n  - name: \"Agent\"\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupContinueYamlConfig(cfgPath) {
		t.Fatal("setupContinueYamlConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// Exactly one top-level models: key — the fix for the duplicate-key bug.
	if n := strings.Count(got, "models:"); n != 1 {
		t.Errorf("expected exactly one top-level models: key, got %d:\n%s", n, got)
	}
	for _, want := range []string{
		"name: My Workspace",
		"version: 1.0",
		`  - title: "Existing Model"`,
		`    model: "gpt-4o"`,
		`    apiBase: "http://existing.example/v1"`,
		`    apiKey: "secret"`,
		"# trailing comment",
		"agents:",
		`  - name: "Agent"`,
		`  - title: "FreeBuff DeepSeek Flash"`,
		`    model: "deepseek/deepseek-v4-flash"`,
		`    apiBase: "http://localhost:3457/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}

	// The FreeBuff item goes at the END of the existing models: list: after
	// the user's last model, before the next top-level key/comment.
	idxFree := strings.Index(got, `  - title: "FreeBuff DeepSeek Flash"`)
	idxExisting := strings.Index(got, `  - title: "Existing Model"`)
	idxAgents := strings.Index(got, "agents:")
	idxComment := strings.Index(got, "# trailing comment")
	if idxFree < 0 || idxExisting < 0 || idxAgents < 0 || idxComment < 0 {
		t.Fatalf("expected markers not found:\n%s", got)
	}
	if idxFree < idxExisting {
		t.Errorf("FreeBuff model inserted before the existing model:\n%s", got)
	}
	if idxFree > idxAgents {
		t.Errorf("FreeBuff model inserted after the next top-level key agents::\n%s", got)
	}
	if idxFree > idxComment {
		t.Errorf("FreeBuff model inserted after the top-level comment:\n%s", got)
	}
}

func TestSetupContinueYamlConfigAppendsWhenNoModelsKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	original := "name: My Workspace\nversion: 1.0\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupContinueYamlConfig(cfgPath) {
		t.Fatal("setupContinueYamlConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if n := strings.Count(got, "models:"); n != 1 {
		t.Errorf("expected exactly one top-level models: key, got %d:\n%s", n, got)
	}
	for _, want := range []string{
		"name: My Workspace",
		"version: 1.0",
		`  - title: "FreeBuff DeepSeek Flash"`,
		`    apiBase: "http://localhost:3457/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestSetupContinueYamlConfigShortCircuitsWhenAlreadyConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	original := "name: My Workspace\nmodels:\n  - title: \"Old\"\n    apiBase: \"http://localhost:3457/v1\"\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupContinueYamlConfig(cfgPath) {
		t.Fatal("setupContinueYamlConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("already-configured file was modified:\n%s", string(out))
	}
}

func TestSetupOpencodeConfigJSONCCommentsLeaveFileUntouched(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "opencode.json")
	// opencode.json allows // comments (JSONC); Go's json package does not.
	// The config must never be rewritten from scratch in that case — the
	// user's providers/agents/MCPs (and API keys) would be deleted.
	original := `{
  // opencode.json accepts JSONC comments; Go's encoding/json does not.
  "provider": {
    "anthropic": {
      "options": { "apiKey": "sk-ant-secret" }
    }
  },
  "agent": { "build": { "prompt": "hi" } }
}
`
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if setupOpencodeConfig(cfgPath) {
		t.Fatal("setupOpencodeConfig should return false for an unparseable JSONC config")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != original {
		t.Errorf("JSONC config was modified; must be left untouched:\n%s", string(out))
	}
}

func TestSetupOpencodeConfigAddsFreebuffProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "opencode.json")
	original := `{"providers": {"anthropic": {"options": {"apiKey": "sk-ant-secret"}}}}`
	if err := os.WriteFile(cfgPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if !setupOpencodeConfig(cfgPath) {
		t.Fatal("setupOpencodeConfig returned false")
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("rewritten opencode.json is not valid JSON: %v\n%s", err, out)
	}
	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		t.Fatalf("rewritten opencode.json lost the providers key:\n%s", out)
	}
	if _, ok := providers["freebuff"]; !ok {
		t.Errorf("freebuff provider missing from rewritten config:\n%s", out)
	}
	if _, ok := providers["anthropic"]; !ok {
		t.Errorf("existing anthropic provider was deleted from rewritten config:\n%s", out)
	}
}
