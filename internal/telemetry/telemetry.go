// Package telemetry provides the leveled color/file logger, redacted header
// copies, and optional request dumps for the freebuff-proxy bridge
// (PRD §3: structured logging — color terminal + file, debug dump mode).
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ANSI 4-color scheme: DEBUG gray, INFO green, WARN yellow, ERROR red.
const (
	ansiGray   = "\x1b[90m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiReset  = "\x1b[0m"
)

// timeFormat mirrors slog's text handler timestamp.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// NewLogger builds the process logger at Info level, or Debug when verbose.
// It is a convenience wrapper over New keeping the original API.
func NewLogger(verbose bool, logFile string) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return New(level, logFile)
}

// ParseLevel parses a LOG_LEVEL-style string into a slog level. The empty
// string returns ok=false (caller falls back to its default).
func ParseLevel(s string) (slog.Level, bool) {
	if s == "" {
		return 0, false
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, false
	}
	return level, true
}

// New builds the process logger at the given level. stderr gets the
// colorized text handler; when logFile is set the same lines are appended
// there via io.MultiWriter. Coloring is disabled only when a log file is
// actually opened — a single handler writes to both sinks and ANSI escapes
// in a file are noise. A log file that cannot be opened is reported on
// stderr and stderr-only logging continues, keeping its colors.
func New(level slog.Level, logFile string) *slog.Logger {
	w := io.Writer(os.Stderr)
	var file *os.File
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: warning: cannot open log file %s: %v\n", logFile, err)
		} else {
			file = f
			w = io.MultiWriter(os.Stderr, f)
		}
	}

	h := &textHandler{w: w, level: level, colorize: file == nil, file: file}
	return slog.New(h)
}

// textHandler is a minimal slog text handler that colorizes the level token
// (time=... level=INFO msg=... key=value...). WithAttrs/WithGroup are no-ops:
// the process logger never carries bound attrs. file is the appended log
// file (nil for stderr-only), kept for tests and a future shutdown close.
type textHandler struct {
	mu       sync.Mutex
	level    slog.Leveler
	w        io.Writer
	colorize bool
	file     *os.File
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	line := fmt.Sprintf("time=%s level=%s msg=%s",
		r.Time.Format(timeFormat), h.levelToken(r.Level), quoteMessage(r.Message))
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

func (h *textHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *textHandler) WithGroup(_ string) slog.Handler      { return h }

// levelToken renders the level marker, colorized unless the sink includes a
// file (ANSI escapes in a log file are noise).
func (h *textHandler) levelToken(level slog.Level) string {
	if !h.colorize {
		return level.String()
	}
	return levelColor(level) + level.String() + ansiReset
}

func levelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return ansiRed
	case level >= slog.LevelWarn:
		return ansiYellow
	case level >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiGray
	}
}

// quoteMessage quotes multi-word messages so one line stays one record.
func quoteMessage(msg string) string {
	if strings.ContainsAny(msg, " \t") {
		return strconv.Quote(msg)
	}
	return msg
}

// sensitiveHeaders are redacted in dumps and request logs; keys are compared
// lower-cased so direct (non-canonical) header assignments are covered too.
var sensitiveHeaders = map[string]struct{}{
	"authorization": {},
	"x-api-key":     {},
	"cookie":        {},
	"set-cookie":    {},
}

// RedactHeaders returns a copy of h with the values of sensitive headers
// (Authorization, x-api-key, Cookie, Set-Cookie) replaced by "[redacted]".
// The input header is not modified.
func RedactHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		copied := make([]string, len(vs))
		copy(copied, vs)
		if _, sensitive := sensitiveHeaders[strings.ToLower(k)]; sensitive {
			for i := range copied {
				copied[i] = "[redacted]"
			}
		}
		out[k] = copied
	}
	return out
}

// sanitizeName makes a request path safe to embed in a dump file name on
// every platform: separators, dots and each character that is invalid in
// Windows file names are replaced with underscores.
func sanitizeName(p string) string {
	for _, r := range `/\:*?"<>|.` {
		p = strings.ReplaceAll(p, string(r), "_")
	}
	if len(p) > 60 {
		p = p[:60]
	}
	return p
}

// truncate shortens s to at most n runes (UTF-8-safe: multi-byte sequences
// are never split) plus an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
