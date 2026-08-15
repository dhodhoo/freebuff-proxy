package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runSetup(autoYes bool) {
	fmt.Println("freebuff-proxy interactive client setup")
	fmt.Println("======================================")
	fmt.Println("This helper detects installed AI tools and offers to configure them.")
	fmt.Println("No files will be modified without your explicit permission.")

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(os.Stderr, "ERROR: cannot determine user home directory: %v\n", err)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)
	ask := func(prompt string) bool {
		if autoYes {
			return true
		}
		fmt.Printf("\n%s [y/N]: ", prompt)
		input, _ := reader.ReadString('\n')
		input = strings.ToLower(strings.TrimSpace(input))
		return input == "y" || input == "yes"
	}

	configured := 0

	// 1. Continue (VS Code / JetBrains)
	continueDir := filepath.Join(home, ".continue")
	continueYamlPath := filepath.Join(continueDir, "config.yaml")
	continueJsonPath := filepath.Join(continueDir, "config.json")
	if fileExists(continueDir) || fileExists(continueYamlPath) || fileExists(continueJsonPath) {
		fmt.Printf("\n[+] Detected Continue extension (~/.continue/)\n")
		targetPath := continueYamlPath
		if !fileExists(continueYamlPath) && fileExists(continueJsonPath) {
			targetPath = continueJsonPath
		}
		if ask(fmt.Sprintf("Would you like to add freebuff-proxy to Continue (%s)?", filepath.Base(targetPath))) {
			if strings.HasSuffix(targetPath, ".yaml") || strings.HasSuffix(targetPath, ".yml") {
				if setupContinueYamlConfig(targetPath) {
					fmt.Printf("    [ok] Configured Continue in %s (backup saved to .bak)\n", targetPath)
					configured++
				}
			} else {
				if setupContinueConfig(targetPath) {
					fmt.Printf("    [ok] Configured Continue in %s (backup saved to .bak)\n", targetPath)
					configured++
				}
			}
		} else {
			fmt.Println("    [skipped] Left Continue config untouched.")
			fmt.Println("    Manual snippet for ~/.continue/config.yaml:")
			fmt.Println("    models:\n      - title: \"FreeBuff DeepSeek\"\n        provider: \"openai\"\n        model: \"deepseek/deepseek-v4-flash\"\n        apiBase: \"http://localhost:3457/v1\"\n        apiKey: \"not-needed\"")
		}
	} else {
		fmt.Println("[-] Continue (~/.continue/) not found on this system")
	}

	// 2. opencode
	opencodeDir := filepath.Join(home, ".config", "opencode")
	opencodeCfgPath := filepath.Join(opencodeDir, "opencode.json")
	if fileExists(opencodeDir) || fileExists(opencodeCfgPath) {
		fmt.Printf("\n[+] Detected opencode (~/.config/opencode/)\n")
		if ask("Would you like to add the freebuff provider to opencode.json?") {
			if setupOpencodeConfig(opencodeCfgPath) {
				fmt.Printf("    [ok] Configured opencode in %s (backup saved to .bak)\n", opencodeCfgPath)
				configured++
			}
		} else {
			fmt.Println("    [skipped] Left opencode config untouched.")
			fmt.Println("    Manual snippet for ~/.config/opencode/opencode.json:")
			fmt.Println(`    "freebuff": {"type": "openai", "options": {"baseURL": "http://localhost:3457/v1", "apiKey": "not-needed"}}`)
		}
	} else {
		fmt.Println("[-] opencode (~/.config/opencode/) not found on this system")
	}

	// 3. aider
	aiderCfgPath := filepath.Join(home, ".aider.conf.yml")
	if _, err := exec.LookPath("aider"); err != nil {
		fmt.Println("[-] aider not found on this system")
	} else if ask("Would you like to configure aider in ~/.aider.conf.yml?") {
		if setupAiderConfig(aiderCfgPath) {
			fmt.Printf("    [ok] Configured aider in %s\n", aiderCfgPath)
			configured++
		}
	} else {
		fmt.Println("    [skipped] Left aider config untouched.")
		fmt.Println("    Manual flags: aider --openai-api-base http://localhost:3457/v1 --openai-api-key not-needed")
	}

	fmt.Printf("\n======================================\n")
	fmt.Printf("Setup complete! Configured %d client tool(s).\n", configured)
	fmt.Println("Base URL: http://localhost:3457/v1")
	// The model list is served live by the proxy; pointing at the endpoint
	// beats maintaining a hardcoded, drifting copy here.
	fmt.Println("Models available: query http://localhost:3457/v1/models for the live list")
	os.Exit(0)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func backupFile(p string) {
	if !fileExists(p) {
		return
	}
	bak := p + ".bak"
	data, err := os.ReadFile(p)
	if err == nil {
		_ = os.WriteFile(bak, data, 0644)
	}
}

func setupContinueYamlConfig(p string) bool {
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)

	backupFile(p)

	snippet := "\nmodels:\n  - title: \"FreeBuff DeepSeek Flash\"\n    provider: \"openai\"\n    model: \"deepseek/deepseek-v4-flash\"\n    apiBase: \"http://localhost:3457/v1\"\n    apiKey: \"not-needed\"\n"
	if fileExists(p) {
		existing, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(existing), "localhost:3457") {
			return true
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return false
		}
		defer func() { _ = f.Close() }()
		_, err = f.WriteString(snippet)
		return err == nil
	}
	return os.WriteFile(p, []byte(snippet), 0644) == nil
}

func setupContinueConfig(p string) bool {
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)

	backupFile(p)

	var cfg map[string]any
	if data, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}

	models, _ := cfg["models"].([]any)
	hasFreebuff := false
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok {
			if apiBase, _ := mm["apiBase"].(string); strings.Contains(apiBase, "3457") {
				hasFreebuff = true
				break
			}
		}
	}

	if !hasFreebuff {
		newModel := map[string]any{
			"title":    "FreeBuff DeepSeek Flash",
			"provider": "openai",
			"model":    "deepseek/deepseek-v4-flash",
			"apiBase":  "http://localhost:3457/v1",
			"apiKey":   "not-needed",
		}
		models = append(models, newModel)
		cfg["models"] = models

		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return false
		}
		return os.WriteFile(p, out, 0644) == nil
	}

	return true
}

func setupOpencodeConfig(p string) bool {
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)

	backupFile(p)

	var cfg map[string]any
	if data, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}

	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		providers = make(map[string]any)
	}

	providers["freebuff"] = map[string]any{
		"type": "openai",
		"options": map[string]any{
			"baseURL": "http://localhost:3457/v1",
			"apiKey":  "not-needed",
		},
		"models": []map[string]any{
			{"id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash"},
			{"id": "z-ai/glm-5.2", "name": "GLM 5.2"},
		},
	}
	cfg["providers"] = providers

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false
	}
	return os.WriteFile(p, out, 0644) == nil
}

func setupAiderConfig(p string) bool {
	newLines := []string{
		"openai-api-base: http://localhost:3457/v1",
		"openai-api-key: not-needed",
		"model: openai/deepseek/deepseek-v4-flash",
	}
	if fileExists(p) {
		existing, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(existing), "localhost:3457") {
			return true
		}
		backupFile(p)
		if err == nil {
			merged := mergeAiderConfig(string(existing), newLines)
			return os.WriteFile(p, []byte(merged), 0644) == nil
		}
	}
	return os.WriteFile(p, []byte(strings.Join(newLines, "\n")+"\n"), 0644) == nil
}

// mergeAiderConfig merges key:value lines into existing YAML-style config
// text, preserving every unrelated line. A key already present (matched by its
// "key:" line prefix) is replaced in place; missing keys are appended at the
// end. The file's original line-ending style is preserved.
func mergeAiderConfig(existing string, lines []string) string {
	nl := "\n"
	if strings.Contains(existing, "\r\n") {
		nl = "\r\n"
	}

	split := strings.Split(existing, "\n")
	// Strip the \r left by CRLF line endings so replacement lines don't end
	// up with doubled \r after the join below.
	for i, l := range split {
		split[i] = strings.TrimSuffix(l, "\r")
	}
	found := make(map[string]bool)
	for _, line := range lines {
		key := line[:strings.Index(line, ":")+1]
		for i, l := range split {
			if strings.HasPrefix(l, key) {
				split[i] = line
				found[key] = true
				break
			}
		}
	}

	var missing []string
	for _, line := range lines {
		if key := line[:strings.Index(line, ":")+1]; !found[key] {
			missing = append(missing, line)
		}
	}

	out := strings.Join(split, nl)
	if len(missing) > 0 {
		if !strings.HasSuffix(out, nl) {
			out += nl
		}
		out += strings.Join(missing, nl) + nl
	}
	return out
}
