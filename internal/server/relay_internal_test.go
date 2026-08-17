package server

// Package-internal relay tests (#83/#59): the SSE grace-flush comment,
// periodic keepalive frames during an upstream pause, and the mid-stream
// death error frame. These drive relayStream directly with scripted readers
// so no network/timing flakiness is involved.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testRelayServer() *Server {
	return &Server{logger: slog.Default()}
}

// TestRelayStreamGraceFlush pins the ": connecting" comment: it must be
// written and flushed before any relayed chunk, so a client-side timeout
// can never fire during a long upstream admission pause.
func TestRelayStreamGraceFlush(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	r := strings.NewReader(
		testutilSSE(`{"id":"chatcmpl-g","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
	)
	s.relayStream(context.Background(), rec, r, &relayStats{}, time.Now())
	body := rec.Body.String()
	if !strings.Contains(body, ": connecting\n\n") {
		t.Errorf("body missing ': connecting' grace comment: %q", truncateStr(body, 300))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("body missing [DONE] terminator")
	}
}

// TestRelayStreamKeepalive verifies a comment frame is emitted every
// keepaliveInterval of relay silence (upstream reasoning pause), and that
// normal chunks suppress it.
func TestRelayStreamKeepalive(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	s := testRelayServer()
	rec := httptest.NewRecorder()
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.relayStream(context.Background(), rec, pr, &relayStats{}, time.Now())
	}()

	// One chunk, then silence.
	_, _ = pw.Write([]byte(testutilSSE(`{"id":"chatcmpl-k","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)))
	time.Sleep(150 * time.Millisecond)
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive\n\n") {
		t.Errorf("body missing keepalive comment frames: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "hi") {
		t.Error("body missing the relayed chunk")
	}
}

// errAfterLineReader yields one SSE line then a transport-style error.
type errAfterLineReader struct {
	once bool
}

func (r *errAfterLineReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		line := testutilSSE(`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)
		return copy(p, line), nil
	}
	return 0, errors.New("connection reset by peer")
}

// TestRelayStreamMidStreamError pins the late-failure contract: when the
// upstream dies mid-stream, the relay mirrors an in-band error frame
// (type upstream_error + the provider/transport message) and then [DONE] —
// never a bare hang and never an HTTP status (headers already flushed).
func TestRelayStreamMidStreamError(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	s.relayStream(context.Background(), rec, &errAfterLineReader{}, &relayStats{}, time.Now())
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Errorf("body missing upstream_error frame: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "upstream stream interrupted: connection reset by peer") {
		t.Errorf("body missing provider message: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "partial") {
		t.Error("body missing the relayed partial chunk before the error")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("body missing [DONE] after the error frame")
	}
}

// testutilSSE renders one data frame (local copy of the SSEEvent helper so
// the internal test package stays dependency-light).
func testutilSSE(data string) string { return "data: " + data + "\n\n" }

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
