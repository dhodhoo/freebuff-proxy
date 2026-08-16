package telemetry

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// closeLogFile closes the log file held by a NewLogger result so TempDir
// cleanup can delete it (Windows refuses to delete open files).
func closeLogFile(t *testing.T, logger *slog.Logger) {
	t.Helper()
	th, ok := logger.Handler().(*textHandler)
	if !ok {
		t.Fatalf("logger handler is %T, want *textHandler", logger.Handler())
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.file != nil {
		_ = th.file.Close()
		th.file = nil
	}
}

// captureStderr reroutes os.Stderr for the duration of fn and returns
// everything written to it. Not safe under t.Parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func TestNewLoggerLevelSelection(t *testing.T) {
	infoFile := filepath.Join(t.TempDir(), "info.log")
	infoLogger := NewLogger(false, infoFile)
	infoLogger.Debug("debug line")
	infoLogger.Info("info line")
	closeLogFile(t, infoLogger)

	data, err := os.ReadFile(infoFile)
	if err != nil {
		t.Fatalf("read %s: %v", infoFile, err)
	}
	got := string(data)
	if !strings.Contains(got, `msg="info line"`) {
		t.Errorf("Info line missing from log file: %q", got)
	}
	if strings.Contains(got, "debug line") {
		t.Errorf("Debug line logged at Info level: %q", got)
	}

	debugFile := filepath.Join(t.TempDir(), "debug.log")
	debugLogger := NewLogger(true, debugFile)
	debugLogger.Debug("debug line")
	closeLogFile(t, debugLogger)

	data, err = os.ReadFile(debugFile)
	if err != nil {
		t.Fatalf("read %s: %v", debugFile, err)
	}
	if !strings.Contains(string(data), `msg="debug line"`) {
		t.Errorf("Debug line missing at Debug level: %q", data)
	}
}

func TestNewLoggerAppendsToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	first := NewLogger(true, path)
	first.Info("first")
	second := NewLogger(true, path)
	second.Info("second")
	closeLogFile(t, first)
	closeLogFile(t, second)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(data)
	if !strings.Contains(got, "msg=first") || !strings.Contains(got, "msg=second") {
		t.Errorf("expected both lines appended, got: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("log file contains ANSI color escapes: %q", got)
	}
}

func TestNewLoggerColors(t *testing.T) {
	out := captureStderr(t, func() {
		logger := NewLogger(true, "")
		logger.Debug("m-debug")
		logger.Info("m-info")
		logger.Warn("m-warn")
		logger.Error("m-error")
	})
	for _, want := range []struct{ msg, token string }{
		{"m-debug", "\x1b[90mDEBUG\x1b[0m"},
		{"m-info", "\x1b[32mINFO\x1b[0m"},
		{"m-warn", "\x1b[33mWARN\x1b[0m"},
		{"m-error", "\x1b[31mERROR\x1b[0m"},
	} {
		if !strings.Contains(out, want.msg) {
			t.Errorf("stderr missing message %q", want.msg)
		}
		if !strings.Contains(out, want.token) {
			t.Errorf("stderr missing color token %q (for %s): %q", want.token, want.msg, out)
		}
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer topsecret")
	h.Add("x-api-key", "key-1")
	h.Add("x-api-key", "key-2")
	h.Set("Cookie", "session=abc")
	h.Set("Set-Cookie", "sid=xyz")
	h.Set("Content-Type", "application/json")
	h.Set("X-Custom", "keepme")

	got := RedactHeaders(h)

	if h.Get("Authorization") != "Bearer topsecret" {
		t.Error("RedactHeaders modified the input header")
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "Set-Cookie"} {
		if v := got[k][0]; v != "[redacted]" {
			t.Errorf("RedactHeaders[%q] = %q, want [redacted]", k, v)
		}
	}
	if v := got["Content-Type"][0]; v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
	if v := got["X-Custom"][0]; v != "keepme" {
		t.Errorf("X-Custom = %q, want keepme", v)
	}
	if vs := got["X-Api-Key"]; len(vs) != 2 || vs[0] != "[redacted]" || vs[1] != "[redacted]" {
		t.Errorf("X-Api-Key values = %v, want two [redacted]", vs)
	}
}

func TestRedactHeadersNonCanonicalKey(t *testing.T) {
	h := http.Header{"x-api-key": {"k"}}
	got := RedactHeaders(h)
	if v := got["x-api-key"][0]; v != "[redacted]" {
		t.Errorf("lowercase raw key value = %q, want [redacted]", v)
	}
}

func TestParseLevel(t *testing.T) {
	if _, ok := ParseLevel(""); ok {
		t.Error(`ParseLevel("") ok=true, want false`)
	}
	if lv, ok := ParseLevel("debug"); !ok || lv != slog.LevelDebug {
		t.Errorf("ParseLevel(debug) = %v, ok=%v; want %v, true", lv, ok, slog.LevelDebug)
	}
	if lv, ok := ParseLevel("INFO"); !ok || lv != slog.LevelInfo {
		t.Errorf("ParseLevel(INFO) = %v, ok=%v; want %v, true", lv, ok, slog.LevelInfo)
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Error("ParseLevel(bogus) ok=true, want false")
	}
}

func TestSanitizeName(t *testing.T) {
	input := `a\b:c*d?e"f<g>h|i`
	got := sanitizeName(input)
	for _, r := range got {
		if strings.ContainsRune(`/\:*?"<>|.`, r) {
			t.Errorf("sanitizeName(%q) = %q, contains invalid file-name char %q", input, got, r)
		}
	}
	if len(got) > 60 {
		t.Errorf("sanitizeName(%q) length = %d, want <= 60", input, len(got))
	}
}

func TestTruncateUTF8Safe(t *testing.T) {
	// 50 multi-byte runes (100 bytes): byte slicing would split a rune.
	s := strings.Repeat("é", 50)
	got := truncate(s, 10)
	if n := len([]rune(got)); n != 13 {
		t.Errorf("truncate(50×é, 10) = %d runes, want 13 (10 + ellipsis)", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate(50×é, 10) = %q, want ellipsis suffix", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncate(50×é, 10) = %q, want valid UTF-8", got)
	}

	if short := truncate("abc", 5); short != "abc" {
		t.Errorf("truncate(abc, 5) = %q, want abc (unchanged)", short)
	}
}

// TestNewLoggerCreatesNestedLogDir verifies that LOG_FILE may point into a
// fresh nested directory: the parent is created before the file is opened.
func TestNewLoggerCreatesNestedLogDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "proxy.log")
	logger := NewLogger(true, path)
	logger.Info("nested-dir line")
	closeLogFile(t, logger)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (parent dir was not created)", path, err)
	}
	if !strings.Contains(string(data), `msg="nested-dir line"`) {
		t.Errorf("line missing from log file in nested dir: %q", data)
	}
}

func TestColorizeWhenLogFileFailsToOpen(t *testing.T) {
	// A log file whose parent cannot be created (a regular file occupies the
	// directory position, so MkdirAll fails) cannot be opened; the logger
	// falls back to stderr-only and must keep its ANSI colors.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		logger := NewLogger(true, filepath.Join(blocker, "x.log"))
		logger.Info("still-colored")
	})
	if !strings.Contains(out, "\x1b[32mINFO\x1b[0m") {
		t.Errorf("stderr lost colors after log file open failure: %q", out)
	}
}

// TestAttrValueEscaping verifies that attr values containing newlines, tabs,
// quotes or other control characters are quoted, so one log record always
// writes exactly one line. Regression: values were concatenated via
// a.Value.String() unescaped, so a client-controlled model name or URL path
// containing "\nlevel=ERROR injected" forged a second log line.
func TestAttrValueEscaping(t *testing.T) {
	var buf bytes.Buffer
	h := &textHandler{w: &buf, level: slog.LevelInfo}
	logger := slog.New(h)
	logger.Info("request handled",
		"model", "codebuff-1\nlevel=ERROR injected",
		"path", "/v1/chat/completions",
		"user_agent", "tab\t\"quoted\"\x1b[31mred",
	)

	out := buf.String()
	// The injected token must survive only in its strconv.Quote-escaped form
	// inside the quoted value, never as a raw newline splitting the record.
	if !strings.Contains(out, `model="codebuff-1\nlevel=ERROR injected"`) {
		t.Errorf("model attr not escaped via strconv.Quote, got: %q", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("record split across %d lines, want exactly 1: %q", n, out)
	}
	if !strings.Contains(out, `user_agent="tab\t\"quoted\"\x1b[31mred"`) {
		t.Errorf("tab/quote/control chars not escaped, got: %q", out)
	}
	if !strings.Contains(out, "path=/v1/chat/completions") {
		t.Errorf("clean attr value must stay unquoted, got: %q", out)
	}
}
