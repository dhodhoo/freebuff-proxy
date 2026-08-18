package updatecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.9.3", "v0.9.3", 0},
		{"v0.9.3", "v0.10.0", -1},
		{"v0.10.0", "v0.9.3", 1},
		{"1.2.3", "v1.2.3", 0},
		{"v1.2", "v1.2.0", 0},        // missing component counts as 0
		{"v1.10.0", "v1.9.9", 1},     // numeric, not lexicographic
		{"v0.9.3", "dev", 0},         // unparseable → 0
		{"", "v0.9.3", 0},            // unparseable → 0
		{"v0.9.3-rc.1", "v0.9.3", 0}, // suffix ignored
		{"v2.0.0", "v1.99.99", 1},
		{"v0.0.9", "v0.0.10", -1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestUpdateAvailable(t *testing.T) {
	if UpdateAvailable("v0.9.3", "v0.9.3") {
		t.Error("same version → update available")
	}
	if !UpdateAvailable("v0.9.3", "v0.9.4") {
		t.Error("newer release → update available")
	}
	if UpdateAvailable("v0.9.4", "v0.9.3") {
		t.Error("older release → update available")
	}
	if UpdateAvailable("dev", "v0.9.4") {
		t.Error("dev build → update available (cannot compare)")
	}
	if UpdateAvailable("", "v0.9.4") {
		t.Error("empty current → update available")
	}
	if UpdateAvailable("v0.9.3", "") {
		t.Error("empty latest → update available")
	}
}

func TestLatestFetchesAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.Contains(r.URL.Path, "/releases/latest") {
			t.Errorf("path = %q, want /releases/latest", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.9.4"}`))
	}))
	defer srv.Close()

	// The checker builds the github.com URL itself; to observe the fetch we
	// need a transport-level redirect hook: wrap the client so the request
	// goes to the test server instead.
	tr := &rewriteTransport{target: srv.URL}
	c := New(DefaultRepo, &http.Client{Transport: tr, Timeout: fetchTimeout})

	latest, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest != "v0.9.4" {
		t.Errorf("latest = %q, want v0.9.4", latest)
	}
	if hits != 1 {
		t.Errorf("fetches = %d, want 1", hits)
	}

	// Cached: a second call within CacheTTL must not hit the network.
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("fetches = %d after cache reuse, want still 1", hits)
	}
}

func TestLatestFetchFailureReturnsprev(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	tr := &rewriteTransport{target: srv.URL}
	c := New(DefaultRepo, &http.Client{Transport: tr, Timeout: fetchTimeout})

	if latest, err := c.Latest(context.Background()); latest != "" || err == nil {
		t.Fatalf("Latest = %q, %v; want empty + error on first failure", latest, err)
	}
}

// TestLatestFirstFetchFailureBacksOffForTTL verifies that a failed first
// fetch still starts the CacheTTL backoff window: the attempt is stamped
// fetched (see the review-P2 comment in Latest), so a second call well
// inside CacheTTL must reuse the recorded failure window instead of
// hitting the network again.
func TestLatestFirstFetchFailureBacksOffForTTL(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	tr := &rewriteTransport{target: srv.URL}
	c := New(DefaultRepo, &http.Client{Transport: tr, Timeout: fetchTimeout})

	if latest, err := c.Latest(context.Background()); latest != "" || err == nil {
		t.Fatalf("first Latest = %q, %v; want empty + error", latest, err)
	}
	if hits != 1 {
		t.Fatalf("network hits after first call = %d, want 1", hits)
	}

	// Second call, immediately (well inside CacheTTL): must NOT re-fetch,
	// and the backoff hit surfaces as an empty tag with no error.
	latest2, err2 := c.Latest(context.Background())
	if latest2 != "" || err2 != nil {
		t.Errorf("second Latest = %q, %v; want empty tag + nil error (backoff hit)", latest2, err2)
	}
	if hits != 1 {
		t.Errorf("network hits after second call = %d, want 1 (first-ever failure must back off for CacheTTL)", hits)
	}
}

// rewriteTransport sends every request to target (a test seam: the checker
// hardcodes the github.com URL, which tests must not contact).
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

var _ = time.Second // keep the time import for future cache-age assertions
