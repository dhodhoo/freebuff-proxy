package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/egress"
)

// TestDoctorEgressProbeParsesTrace guards the doctor's region probe: a
// direct probe against a local trace-style endpoint must return the parsed
// ip= and loc= values, the same parse the region row reports. The probe
// URL is redirected via egress.ProbeURL so no external network is touched.
func TestDoctorEgressProbeParsesTrace(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/trace" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ip=203.0.113.7\nloc=JP\n"))
	}))
	defer ts.Close()

	orig := egress.ProbeURL
	egress.ProbeURL = ts.URL + "/cdn-cgi/trace"
	defer func() { egress.ProbeURL = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := egress.Probe(ctx, egress.DirectDialer(5*time.Second), 5*time.Second)
	if res.Err != nil {
		t.Fatalf("direct probe failed: %v", res.Err)
	}
	if res.IP != "203.0.113.7" || res.Country != "JP" {
		t.Errorf("Probe = ip %q loc %q, want 203.0.113.7 / JP", res.IP, res.Country)
	}
}

// TestEgressRegionRow guards the doctor row rendering: a cached direct
// result prints "Egress region: <country> (<ip>)", while a failed or
// missing probe prints an "(unavailable)" warning line.
func TestEgressRegionRow(t *testing.T) {
	c := egress.NewCache()
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	line, warn := egressRegionRow(c)
	if warn || line != "Egress region: US (1.2.3.4)" {
		t.Errorf("row = %q (warn=%v), want success row", line, warn)
	}

	c = egress.NewCache()
	c.Set("direct", egress.Result{Err: context.DeadlineExceeded})
	line, warn = egressRegionRow(c)
	if !warn || !strings.Contains(line, "unavailable") || !strings.Contains(line, "context deadline exceeded") {
		t.Errorf("failed-probe row = %q (warn=%v), want unavailable warning with reason", line, warn)
	}

	c = egress.NewCache() // no entry at all
	line, warn = egressRegionRow(c)
	if !warn || !strings.Contains(line, "unavailable") {
		t.Errorf("empty-cache row = %q (warn=%v), want unavailable warning", line, warn)
	}
}
