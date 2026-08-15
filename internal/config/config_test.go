package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envKeys lists every environment variable the package reads. Tests clear
// them all first so machine-level env (e.g. a corporate HTTP_PROXY) can never
// leak into assertions.
var envKeys = []string{
	"LISTEN_ADDR", "UPSTREAM_BASE_URL", "AUTH_TOKENS", "ROTATION_INTERVAL",
	"REQUEST_TIMEOUT", "SESSION_CALL_TIMEOUT", "API_KEYS", "HTTP_PROXY",
	"SOCKS5_PROXY", "SOCKS5_PROXIES", "COST_MODE", "TLS_FINGERPRINT", "REGISTRY_REFRESH", "DEBUG_DUMP", "LOG_FILE", "LOG_LEVEL",
	"MAX_MESSAGES_PER_DAY", "IDLE_ROTATION_TIMEOUT", "SAFE_MODE", "REQUEST_JITTER", "CLI_VERSION", "MODEL_ALIASES", "AUTO_DISCOVER_TOKEN",
	"TRANSIENT_RETRIES",
}

func clearEnv(t *testing.T) {
	t.Helper()
	// Isolate from any real ./.env in the working directory (the repo ships
	// a gitignored .env with real tokens) — Load() reads it by default.
	t.Chdir(t.TempDir())
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	t.Setenv("AUTO_DISCOVER_TOKEN", "false")
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:3457" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "127.0.0.1:3457")
	}
	if cfg.UpstreamBaseURL != "https://www.codebuff.com" {
		t.Errorf("UpstreamBaseURL = %q, want %q", cfg.UpstreamBaseURL, "https://www.codebuff.com")
	}
	if want := []string{"tok-1"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v", cfg.AuthTokens, want)
	}
	if cfg.RotationInterval != 6*time.Hour {
		t.Errorf("RotationInterval = %v, want 6h", cfg.RotationInterval)
	}
	if cfg.RequestTimeout != 15*time.Minute {
		t.Errorf("RequestTimeout = %v, want 15m", cfg.RequestTimeout)
	}
	if cfg.SessionCallTimeout != 30*time.Second {
		t.Errorf("SessionCallTimeout = %v, want 30s", cfg.SessionCallTimeout)
	}
	if cfg.RegistryRefresh != 6*time.Hour {
		t.Errorf("RegistryRefresh = %v, want 6h", cfg.RegistryRefresh)
	}
	if cfg.DebugDump {
		t.Error("DebugDump = true, want false")
	}
	if !cfg.SafeMode {
		t.Error("SafeMode = false, want true (default)")
	}
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty", cfg.LogFile)
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want empty", cfg.LogLevel)
	}
	if cfg.HTTPProxy != "" || cfg.SOCKS5Proxy != "" {
		t.Errorf("proxies = %q/%q, want empty", cfg.HTTPProxy, cfg.SOCKS5Proxy)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (default: omission routes requests as paid -> 402)", cfg.CostMode)
	}
}

func TestTransientRetries(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	// default: 1 (one additional attempt after a transient transport failure)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.TransientRetries != 1 {
		t.Errorf("TransientRetries = %d, want 1 (default)", cfg.TransientRetries)
	}

	// explicit 0 disables retries
	t.Setenv("TRANSIENT_RETRIES", "0")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (0): %v", err)
	} else if cfg.TransientRetries != 0 {
		t.Errorf("TransientRetries = %d, want 0 (disabled)", cfg.TransientRetries)
	}
	t.Setenv("TRANSIENT_RETRIES", "")

	// JSON file value
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"TRANSIENT_RETRIES": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (JSON): %v", err)
	} else if cfg.TransientRetries != 2 {
		t.Errorf("TransientRetries = %d, want 2 (JSON)", cfg.TransientRetries)
	}

	// negative fails validation
	t.Setenv("TRANSIENT_RETRIES", "-1")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "TRANSIENT_RETRIES") {
		t.Fatalf("Load (negative): err = %v, want error mentioning TRANSIENT_RETRIES", err)
	}
	t.Setenv("TRANSIENT_RETRIES", "")
}

func TestSafeMode(t *testing.T) {
	t.Run("default SafeMode values", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "true")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.MaxMessagesPerDay != 150 {
			t.Errorf("MaxMessagesPerDay = %d, want 150 under SafeMode", cfg.MaxMessagesPerDay)
		}
		if cfg.IdleRotationTimeout != 30*time.Minute {
			t.Errorf("IdleRotationTimeout = %v, want 30m under SafeMode", cfg.IdleRotationTimeout)
		}
		if cfg.RequestJitter != 2*time.Second {
			t.Errorf("RequestJitter = %v, want 2s under SafeMode", cfg.RequestJitter)
		}
	})

	t.Run("explicit zero MaxMessagesPerDay under SafeMode", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "true")
		t.Setenv("MAX_MESSAGES_PER_DAY", "0")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.MaxMessagesPerDay != 0 {
			t.Errorf("MaxMessagesPerDay = %d, want 0 (explicit unlimited)", cfg.MaxMessagesPerDay)
		}
		if cfg.RequestJitter != 2*time.Second {
			t.Errorf("RequestJitter = %v, want 2s under SafeMode", cfg.RequestJitter)
		}
	})

	t.Run("explicit zero knobs under SafeMode stay disabled", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "true")
		t.Setenv("IDLE_ROTATION_TIMEOUT", "0")
		t.Setenv("REQUEST_JITTER", "0s")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.IdleRotationTimeout != 0 {
			t.Errorf("IdleRotationTimeout = %v, want 0 (explicit 0 beats the preset)", cfg.IdleRotationTimeout)
		}
		if cfg.RequestJitter != 0 {
			t.Errorf("RequestJitter = %v, want 0 (explicit 0 beats the preset)", cfg.RequestJitter)
		}
		// Unset knobs still get the presets.
		if cfg.MaxMessagesPerDay != 150 {
			t.Errorf("MaxMessagesPerDay = %d, want 150 under SafeMode", cfg.MaxMessagesPerDay)
		}
		if cfg.TLSFingerprint != "auto" {
			t.Errorf("TLSFingerprint = %q, want auto under SafeMode", cfg.TLSFingerprint)
		}
	})

	t.Run("SAFE_MODE=false restores non-safe defaults", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok-1")
		t.Setenv("SAFE_MODE", "false")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.MaxMessagesPerDay != 0 {
			t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited)", cfg.MaxMessagesPerDay)
		}
		if cfg.IdleRotationTimeout != 0 {
			t.Errorf("IdleRotationTimeout = %v, want 0 (disabled)", cfg.IdleRotationTimeout)
		}
		if cfg.RequestJitter != 0 {
			t.Errorf("RequestJitter = %v, want 0 (disabled)", cfg.RequestJitter)
		}
		if cfg.TLSFingerprint != "" {
			t.Errorf("TLSFingerprint = %q, want empty", cfg.TLSFingerprint)
		}
	})
}

func TestValidationFixSuggestions(t *testing.T) {
	t.Run("Bearer prefix", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "Bearer token123")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "starts with 'Bearer ' prefix") {
			t.Errorf("expected Bearer prefix error, got: %v", err)
		}
	})

	t.Run("Placeholder token", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "cb_xxx")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("expected placeholder error, got: %v", err)
		}
	})

	t.Run("ListenAddr missing colon", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("AUTH_TOKENS", "tok1")
		t.Setenv("LISTEN_ADDR", "3457")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "missing port separator ':'") {
			t.Errorf("expected missing port separator error, got: %v", err)
		}
	})
}

func TestLoadNoTokensBridgeMode(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with no AUTH_TOKENS: %v", err)
	}
	if len(cfg.AuthTokens) != 0 {
		t.Errorf("AuthTokens = %v, want empty (bridge mode)", cfg.AuthTokens)
	}
	if !cfg.BridgeMode() {
		t.Error("BridgeMode() = false, want true with no tokens")
	}
}

func TestLoadFromFile(t *testing.T) {
	clearEnv(t)

	json := `{
		"LISTEN_ADDR": ":9999",
		"UPSTREAM_BASE_URL": "https://codebuff.com",
		"AUTH_TOKENS": ["tok-a", "tok-b"],
		"ROTATION_INTERVAL": "1h",
		"REQUEST_TIMEOUT": "5m",
		"SESSION_CALL_TIMEOUT": "10s",
		"API_KEYS": ["k1"],
		"HTTP_PROXY": "http://proxy.example:3128",
		"SOCKS5_PROXY": "socks5://socks.example:1080",
		"COST_MODE": "free",
		"REGISTRY_REFRESH": "2h",
		"DEBUG_DUMP": true,
		"LOG_FILE": "proxy.log"
	}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999", cfg.ListenAddr)
	}
	if cfg.UpstreamBaseURL != "https://www.codebuff.com" {
		t.Errorf("UpstreamBaseURL = %q, want https://www.codebuff.com", cfg.UpstreamBaseURL)
	}
	if want := []string{"tok-a", "tok-b"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v", cfg.AuthTokens, want)
	}
	if cfg.RotationInterval != time.Hour {
		t.Errorf("RotationInterval = %v, want 1h", cfg.RotationInterval)
	}
	if cfg.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v, want 5m", cfg.RequestTimeout)
	}
	if cfg.SessionCallTimeout != 10*time.Second {
		t.Errorf("SessionCallTimeout = %v, want 10s", cfg.SessionCallTimeout)
	}
	if want := []string{"k1"}; !equalStrings(cfg.APIKeys, want) {
		t.Errorf("APIKeys = %v, want %v", cfg.APIKeys, want)
	}
	if cfg.HTTPProxy != "http://proxy.example:3128" {
		t.Errorf("HTTPProxy = %q", cfg.HTTPProxy)
	}
	if cfg.SOCKS5Proxy != "socks5://socks.example:1080" {
		t.Errorf("SOCKS5Proxy = %q", cfg.SOCKS5Proxy)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free", cfg.CostMode)
	}
	if cfg.RegistryRefresh != 2*time.Hour {
		t.Errorf("RegistryRefresh = %v, want 2h", cfg.RegistryRefresh)
	}
	if !cfg.DebugDump {
		t.Error("DebugDump = false, want true")
	}
	if cfg.LogFile != "proxy.log" {
		t.Errorf("LogFile = %q, want proxy.log", cfg.LogFile)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)

	path := filepath.Join(t.TempDir(), "config.json")
	json := `{
		"LISTEN_ADDR": ":9999",
		"UPSTREAM_BASE_URL": "https://codebuff.com",
		"AUTH_TOKENS": ["file-a"],
		"ROTATION_INTERVAL": "1h",
		"COST_MODE": "free"
	}`
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AUTH_TOKENS", "env-a, env-b ,env-a")
	t.Setenv("LISTEN_ADDR", ":7777")
	t.Setenv("ROTATION_INTERVAL", "90m")
	t.Setenv("DEBUG_DUMP", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != ":7777" {
		t.Errorf("ListenAddr = %q, want :7777 (env wins)", cfg.ListenAddr)
	}
	if want := []string{"env-a", "env-b"}; !equalStrings(cfg.AuthTokens, want) {
		t.Errorf("AuthTokens = %v, want %v (env wins, trimmed, deduped)", cfg.AuthTokens, want)
	}
	if cfg.RotationInterval != 90*time.Minute {
		t.Errorf("RotationInterval = %v, want 90m (env wins)", cfg.RotationInterval)
	}
	if !cfg.DebugDump {
		t.Error("DebugDump = false, want true (env bool wins)")
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free (file value kept)", cfg.CostMode)
	}
}

func TestEnvOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "a,b")
	t.Setenv("UPSTREAM_BASE_URL", "https://codebuff.com/")
	t.Setenv("SESSION_CALL_TIMEOUT", "45s")
	t.Setenv("DEBUG_DUMP", "off")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamBaseURL != "https://www.codebuff.com" {
		t.Errorf("UpstreamBaseURL = %q", cfg.UpstreamBaseURL)
	}
	if cfg.SessionCallTimeout != 45*time.Second {
		t.Errorf("SessionCallTimeout = %v, want 45s", cfg.SessionCallTimeout)
	}
	if cfg.DebugDump {
		t.Error("DebugDump = true, want false (off)")
	}
}

// TestSOCKS5ProxiesEnv verifies the comma-separated SOCKS5_PROXIES env var
// lands in cfg.SOCKS5Proxies. Regression: only the JSON config file
// populated the field, so per-token proxy binding silently fell back to
// SOCKS5Proxy despite the README documenting SOCKS5_PROXIES as env-settable.
func TestSOCKS5ProxiesEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("SOCKS5_PROXIES", "host1:9050,host2:9050")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"host1:9050", "host2:9050"}
	if !equalStrings(cfg.SOCKS5Proxies, want) {
		t.Errorf("SOCKS5Proxies = %v, want %v (from env)", cfg.SOCKS5Proxies, want)
	}
}

// TestDotenvSOCKS5Proxies verifies SOCKS5_PROXIES in ./.env lands in
// cfg.SOCKS5Proxies, mirroring the env override.
func TestDotenvSOCKS5Proxies(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("SOCKS5_PROXIES=host1:9050,host2:9050\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"host1:9050", "host2:9050"}
	if !equalStrings(cfg.SOCKS5Proxies, want) {
		t.Errorf("SOCKS5Proxies = %v, want %v (from .env)", cfg.SOCKS5Proxies, want)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, b ,,c , d\n e\r f ")
	want := []string{"a", "b", "c", "d", "e", "f"}
	if !equalStrings(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"a", " a ", "b", "", "b"})
	want := []string{"a", "b"}
	if !equalStrings(got, want) {
		t.Errorf("dedupeStrings = %v, want %v", got, want)
	}
}

func TestNormalizeUpstreamBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"bare host normalized", "https://codebuff.com", "https://www.codebuff.com", false},
		{"bare host + slash", "https://codebuff.com/", "https://www.codebuff.com", false},
		{"case-insensitive host", "https://CODEBUFF.COM", "https://www.codebuff.com", false},
		{"www kept", "https://www.codebuff.com", "https://www.codebuff.com", false},
		{"www kept + path", "https://www.codebuff.com/api", "https://www.codebuff.com/api", false},
		{"host with port untouched (reference parity)", "https://codebuff.com:8443/x/", "https://codebuff.com:8443/x", false},
		{"other host untouched", "https://api.example.com", "https://api.example.com", false},
		{"no scheme", "codebuff.com", "", true},
		{"bad scheme", "ftp://codebuff.com", "", true},
		{"unparseable", "https://exa mple.com", "", true},
		{"no host", "https://", "", true},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeUpstreamBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeUpstreamBaseURL(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeUpstreamBaseURL(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("normalizeUpstreamBaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	good := Config{
		ListenAddr:         ":3457",
		UpstreamBaseURL:    "https://www.codebuff.com",
		AuthTokens:         []string{"tok"},
		RotationInterval:   6 * time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 30 * time.Second,
		RegistryRefresh:    6 * time.Hour,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config Validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty url", func(c *Config) { c.UpstreamBaseURL = "" }},
		{"unparseable url", func(c *Config) { c.UpstreamBaseURL = "https://exa mple.com" }},
		{"non-http scheme", func(c *Config) { c.UpstreamBaseURL = "ftp://codebuff.com" }},
		{"hostless url", func(c *Config) { c.UpstreamBaseURL = "https://" }},
		{"empty listen addr", func(c *Config) { c.ListenAddr = "" }},
		{"zero rotation", func(c *Config) { c.RotationInterval = 0 }},
		{"zero request timeout", func(c *Config) { c.RequestTimeout = 0 }},
		{"zero session timeout", func(c *Config) { c.SessionCallTimeout = 0 }},
		{"zero registry refresh", func(c *Config) { c.RegistryRefresh = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := good
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Error("Validate succeeded, want error")
			}
		})
	}
}

func TestDotenv(t *testing.T) {
	clearEnv(t) // chdirs to a fresh temp dir; .env is written relative to it

	content := strings.Join([]string{
		"# comment",
		"",
		"AUTH_TOKENS=from-dotenv",
		`LISTEN_ADDR=":9999"`,
		"COST_MODE=free",
		"DEBUG_DUMP=true",
	}, "\n")
	if err := os.WriteFile(".env", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-dotenv" {
		t.Errorf("AuthTokens = %v, want [from-dotenv] (from .env)", got)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999 (quoted value from .env)", cfg.ListenAddr)
	}
	if cfg.CostMode != "free" {
		t.Errorf("CostMode = %q, want free", cfg.CostMode)
	}
	if !cfg.DebugDump {
		t.Error("DebugDump = false, want true")
	}
}

func TestDotenvEnvWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=from-dotenv\nLISTEN_ADDR=:1111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_TOKENS", "from-env")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-env" {
		t.Errorf("AuthTokens = %v, want [from-env] (env beats .env)", got)
	}
	if cfg.ListenAddr != ":1111" {
		t.Errorf("ListenAddr = %q, want :1111 (from .env, env does not set it)", cfg.ListenAddr)
	}
}

func TestDotenvJSONWins(t *testing.T) {
	clearEnv(t)

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("cfg.json", []byte(`{"AUTH_TOKENS":["from-json"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// .env is an environment file: it wins over the JSON config, matching
	// the README rule "environment overrides the JSON config file".
	cfg, err := Load("cfg.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "from-dotenv" {
		t.Errorf("AuthTokens = %v, want [from-dotenv] (.env beats JSON)", got)
	}
}

func TestDotenvMissingIsFine(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthTokens; len(got) != 1 || got[0] != "tok-1" {
		t.Errorf("AuthTokens = %v, want [tok-1]", got)
	}
}

func TestBadFile(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load of malformed JSON succeeded, want error")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load of missing file succeeded, want error")
	}
}

func TestBadDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")
	t.Setenv("ROTATION_INTERVAL", "soon")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "ROTATION_INTERVAL") {
		t.Fatalf("Load with bad duration: err = %v, want parse error mentioning ROTATION_INTERVAL", err)
	}
}

func TestLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: empty when unset
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want empty by default", cfg.LogLevel)
	}

	// env source
	t.Setenv("LOG_LEVEL", "debug")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env debug): %v", err)
	} else if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}

	// invalid level fails validation
	t.Setenv("LOG_LEVEL", "bogus")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("Load (invalid level): err = %v, want error mentioning LOG_LEVEL", err)
	}

	// .env source
	t.Setenv("LOG_LEVEL", "")
	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok\nLOG_LEVEL=warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (.env): %v", err)
	} else if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (from .env)", cfg.LogLevel)
	}
}

func TestTLSFingerprint(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: SAFE_MODE preset (auto) when unset
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (default): %v", err)
	} else if cfg.TLSFingerprint != "auto" {
		t.Errorf("TLSFingerprint = %q, want auto (SAFE_MODE default preset)", cfg.TLSFingerprint)
	}

	// SAFE_MODE=false leaves it empty
	t.Setenv("SAFE_MODE", "false")
	t.Setenv("TLS_FINGERPRINT", "")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (SAFE_MODE=false): %v", err)
	} else if cfg.TLSFingerprint != "" {
		t.Errorf("TLSFingerprint = %q, want empty with SAFE_MODE=false", cfg.TLSFingerprint)
	}
	t.Setenv("SAFE_MODE", "")

	// valid values load OK
	for _, v := range []string{"chrome120", "safari17", "firefox120", "random"} {
		t.Setenv("TLS_FINGERPRINT", v)
		if cfg, err := Load(""); err != nil {
			t.Fatalf("Load (TLSFingerprint=%s): %v", v, err)
		} else if cfg.TLSFingerprint != v {
			t.Errorf("TLSFingerprint = %q, want %s", cfg.TLSFingerprint, v)
		}
	}

	// invalid value fails validation
	t.Setenv("TLS_FINGERPRINT", "bogus")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "TLS_FINGERPRINT") {
		t.Fatalf("Load (invalid TLSFingerprint): err = %v, want error mentioning TLS_FINGERPRINT", err)
	}
}

func TestMaxMessagesPerDay(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: 150 via the SAFE_MODE preset (unset)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.MaxMessagesPerDay != 150 {
		t.Errorf("MaxMessagesPerDay = %d, want 150 (SAFE_MODE default preset)", cfg.MaxMessagesPerDay)
	}

	// SAFE_MODE=false restores unlimited
	t.Setenv("SAFE_MODE", "false")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (SAFE_MODE=false): %v", err)
	} else if cfg.MaxMessagesPerDay != 0 {
		t.Errorf("MaxMessagesPerDay = %d, want 0 (unlimited with SAFE_MODE=false)", cfg.MaxMessagesPerDay)
	}
	t.Setenv("SAFE_MODE", "")

	// env override
	t.Setenv("MAX_MESSAGES_PER_DAY", "25")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env): %v", err)
	} else if cfg.MaxMessagesPerDay != 25 {
		t.Errorf("MaxMessagesPerDay = %d, want 25 (env)", cfg.MaxMessagesPerDay)
	}

	// unparseable env value is ignored (keeps the file value)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"MAX_MESSAGES_PER_DAY": 3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAX_MESSAGES_PER_DAY", "soon")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (bad env + file): %v", err)
	} else if cfg.MaxMessagesPerDay != 3 {
		t.Errorf("MaxMessagesPerDay = %d, want 3 (bad env ignored, file kept)", cfg.MaxMessagesPerDay)
	}

	// JSON file value
	t.Setenv("MAX_MESSAGES_PER_DAY", "")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (file): %v", err)
	} else if cfg.MaxMessagesPerDay != 3 {
		t.Errorf("MaxMessagesPerDay = %d, want 3 (file)", cfg.MaxMessagesPerDay)
	}
}

func TestIdleRotationTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok")

	// default: 30m via the SAFE_MODE preset (unset)
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.IdleRotationTimeout != 30*time.Minute {
		t.Errorf("IdleRotationTimeout = %v, want 30m (SAFE_MODE default preset)", cfg.IdleRotationTimeout)
	}

	// env override
	t.Setenv("IDLE_ROTATION_TIMEOUT", "90m")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env): %v", err)
	} else if cfg.IdleRotationTimeout != 90*time.Minute {
		t.Errorf("IdleRotationTimeout = %v, want 90m (env)", cfg.IdleRotationTimeout)
	}

	// explicit "0" disables
	t.Setenv("IDLE_ROTATION_TIMEOUT", "0")
	if cfg, err := Load(""); err != nil {
		t.Fatalf("Load (env 0): %v", err)
	} else if cfg.IdleRotationTimeout != 0 {
		t.Errorf("IdleRotationTimeout = %v, want 0 (explicit 0)", cfg.IdleRotationTimeout)
	}

	// empty string in JSON is tolerated as disabled (under SAFE_MODE=false;
	// with the default SAFE_MODE=true an empty value is "unset" → preset)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"IDLE_ROTATION_TIMEOUT": ""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IDLE_ROTATION_TIMEOUT", "")
	t.Setenv("SAFE_MODE", "false")
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (empty file): %v", err)
	} else if cfg.IdleRotationTimeout != 0 {
		t.Errorf("IdleRotationTimeout = %v, want 0 (empty tolerated)", cfg.IdleRotationTimeout)
	}
	t.Setenv("SAFE_MODE", "")

	// JSON file value
	if err := os.WriteFile(path, []byte(`{"IDLE_ROTATION_TIMEOUT": "2h"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load (file): %v", err)
	} else if cfg.IdleRotationTimeout != 2*time.Hour {
		t.Errorf("IdleRotationTimeout = %v, want 2h (file)", cfg.IdleRotationTimeout)
	}

	// invalid value fails
	t.Setenv("IDLE_ROTATION_TIMEOUT", "soon")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "IDLE_ROTATION_TIMEOUT") {
		t.Fatalf("Load (bad): err = %v, want parse error mentioning IDLE_ROTATION_TIMEOUT", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseMap(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "empty",
			raw:  "",
			want: map[string]string{},
		},
		{
			name: "single pair",
			raw:  "gpt-4o:deepseek/deepseek-v4-flash",
			want: map[string]string{"gpt-4o": "deepseek/deepseek-v4-flash"},
		},
		{
			name: "multiple pairs with spaces and newlines",
			raw:  " gpt-4o : deepseek/deepseek-v4-flash , \n glm: z-ai/glm-5.2 \n",
			want: map[string]string{
				"gpt-4o": "deepseek/deepseek-v4-flash",
				"glm":    "z-ai/glm-5.2",
			},
		},
		{
			name: "malformed pair skipped",
			raw:  "valid:model,novalue,also:ok",
			want: map[string]string{"valid": "model", "also": "ok"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMap(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseMap(%q) len = %d, want %d", tc.raw, len(got), len(tc.want))
			}
			for k, wantVal := range tc.want {
				if got[k] != wantVal {
					t.Errorf("got[%q] = %q, want %q", k, got[k], wantVal)
				}
			}
		})
	}
}

func TestModelAliasesConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTH_TOKENS", "tok-1")
	t.Setenv("MODEL_ALIASES", "gpt-4o:deepseek/deepseek-v4-flash,glm:z-ai/glm-5.2")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ModelAliases) != 2 {
		t.Fatalf("ModelAliases len = %d, want 2", len(cfg.ModelAliases))
	}
	if cfg.ModelAliases["gpt-4o"] != "deepseek/deepseek-v4-flash" {
		t.Errorf("ModelAliases[gpt-4o] = %q, want deepseek/deepseek-v4-flash", cfg.ModelAliases["gpt-4o"])
	}
}
