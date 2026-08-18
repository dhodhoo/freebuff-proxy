package server_test

import (
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/internal/testutil"
)

// TestMetricsRateLimitEvents pins T7's metrics surface: a classified 429
// chat renders freebuff_proxy_rate_limit_events_total with the token label.
func TestMetricsRateLimitEvents(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusTooManyRequests
	mock.ChatErrorBody = `{"error":"free_mode_rate_limited","message":"wait 30 minutes","retryAfterMs":1800000}`
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		"# HELP freebuff_proxy_rate_limit_events_total",
		`freebuff_proxy_rate_limit_events_total{token="1",code="free_mode_rate_limited"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsRateLimitEventsLabelEscaping mirrors TestMetricsLabelEscaping
// for the code label: quotes in upstream-derived codes are escaped so the
// Prometheus text format stays parseable.
func TestMetricsRateLimitEventsLabelEscaping(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusTooManyRequests
	mock.ChatErrorBody = `{"error":"weird\"code","message":"x"}`
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	want := `freebuff_proxy_rate_limit_events_total{token="1",code="weird\"code"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metrics missing escaped label %s in:\n%s", want, body)
	}
}
