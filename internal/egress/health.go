package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proxy health defaults (mirroring the reference freebuff-reverse proxypool
// checker: reference/freebuff-reverse/internal/proxypool/checker.go).
const (
	// DefaultHealthInterval is how often the background checker re-probes
	// every configured SOCKS5 proxy.
	DefaultHealthInterval = time.Minute
	// DefaultHealthTimeout bounds a single proxy health probe end to end.
	DefaultHealthTimeout = 10 * time.Second
	// DefaultHealthConcurrency bounds simultaneous health probes (the
	// checker never spawns unbounded goroutines).
	DefaultHealthConcurrency = 5
	// DefaultHealthMaxFailures is how many CONSECUTIVE failed probes mark a
	// proxy out-of-rotation (auto-degrade).
	DefaultHealthMaxFailures = 3
)

// HealthProbeURL is the health probe target: a JSON endpoint reporting the
// caller's exit IP plus country/region/city (ip-api.com shape). Exported so
// tests can point the checker at a local server; production never changes
// it. The plain-http default mirrors the reference checker's probe.
var HealthProbeURL = "http://ip-api.com/json/?fields=status,message,country,regionName,city,query"

// ProxyHealthRecord is one proxy's health state, as surfaced by Snapshot
// for the dashboard/diag. ProxyURL is redacted (credentials masked).
type ProxyHealthRecord struct {
	// Addr is the normalized host:port identity key (the proxy endpoint).
	Addr string
	// ProxyURL is the redacted raw URL the proxy was configured with
	// (empty when the record came from a bare host:port).
	ProxyURL     string
	LatencyMS    int64
	FailureCount int
	LastError    string
	ExitIP       string
	Country      string
	Region       string
	City         string
	LastOKAt     time.Time
	Degraded     bool // out-of-rotation: maxFailures consecutive failures
}

// proxyHealthEntry is the mutable per-proxy state behind a record. The
// record's FailureCount IS the consecutive-failure counter: any success
// resets it to zero.
type proxyHealthEntry struct {
	record ProxyHealthRecord
}

// ProxyHealth is a thread-safe per-proxy health registry with auto-degrade:
// maxFailures consecutive probe failures mark a proxy out-of-rotation
// (IsHealthy false, Degraded true); any success recovers it immediately.
// Proxies are deduplicated by their normalized host:port, so the same
// endpoint configured twice (with or without credentials) is probed once.
type ProxyHealth struct {
	mu          sync.Mutex
	maxFailures int
	entries     map[string]*proxyHealthEntry
}

// NewProxyHealth returns an empty registry; maxFailures <= 0 falls back to
// DefaultHealthMaxFailures.
func NewProxyHealth(maxFailures int) *ProxyHealth {
	if maxFailures <= 0 {
		maxFailures = DefaultHealthMaxFailures
	}
	return &ProxyHealth{maxFailures: maxFailures, entries: make(map[string]*proxyHealthEntry)}
}

// Add registers raw (a bare host:port or socks5:// URL) for health checking.
// Duplicate endpoints (same normalized host:port) are ignored. Returns the
// normalized addr and whether the entry was newly added. Invalid addresses
// return ("", false) without error so callers can skip-and-warn (fail-open).
func (h *ProxyHealth) Add(raw string) (string, bool) {
	if h == nil {
		return "", false
	}
	addr, _, err := parseSocks5(raw)
	if err != nil || addr == "" {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.entries[addr]; ok {
		return addr, false
	}
	h.entries[addr] = &proxyHealthEntry{
		record: ProxyHealthRecord{Addr: addr, ProxyURL: redactProxyURL(strings.TrimSpace(raw))},
	}
	return addr, true
}

// RecordSuccess marks a healthy probe: latency, exit geo, and a reset of the
// consecutive-failure counter (the proxy recovers immediately).
func (h *ProxyHealth) RecordSuccess(addr string, latencyMS int64, ip, country, region, city string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[addr]
	if !ok {
		return
	}
	e.record.FailureCount = 0
	e.record.LatencyMS = latencyMS
	e.record.ExitIP = ip
	e.record.Country = country
	e.record.Region = region
	e.record.City = city
	e.record.LastError = ""
	e.record.LastOKAt = time.Now()
	e.record.Degraded = false
}

// RecordFailure marks a failed probe, advancing the consecutive-failure
// counter. Once it reaches the configured maximum the proxy is degraded
// (out-of-rotation).
func (h *ProxyHealth) RecordFailure(addr string, err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[addr]
	if !ok {
		return
	}
	e.record.FailureCount++
	if err != nil {
		e.record.LastError = err.Error()
	}
	e.record.Degraded = e.record.FailureCount >= h.maxFailures
}

// IsHealthy reports whether addr's proxy is in rotation: registered, not
// degraded. Unknown addresses report true so a dialer without the registry
// entry (or with a registry configured after the dialers were built) never
// blocks a proxy it knows nothing about.
func (h *ProxyHealth) IsHealthy(addr string) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[addr]
	if !ok {
		return true
	}
	return !e.record.Degraded
}

// Snapshot returns a copy of every tracked proxy's health record, sorted by
// address for stable output.
func (h *ProxyHealth) Snapshot() []ProxyHealthRecord {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ProxyHealthRecord, 0, len(h.entries))
	for _, e := range h.entries {
		out = append(out, e.record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// healthProbeResult is the parsed ip-api-style probe response.
type healthProbeResult struct {
	IP      string
	Country string
	Region  string
	City    string
}

// probeHealth GETs url through dialer, bounded by timeout, and parses the
// ip-api JSON shape (status/message/country/regionName/city/query). A
// non-2xx status, an unreadable body, or a {"status":"fail"} payload returns
// an error carrying the upstream message.
func probeHealth(ctx context.Context, dialer func(ctx context.Context, network, addr string) (net.Conn, error), url string, timeout time.Duration) (healthProbeResult, error) {
	if dialer == nil {
		dialer = DirectDialer(timeout)
	}
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	tr := &http.Transport{
		DialContext:         dialer,
		MaxIdleConns:        1,
		IdleConnTimeout:     DefaultTTL,
		TLSHandshakeTimeout: timeout,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return healthProbeResult{}, err
	}
	req.Header.Set("User-Agent", "freebuff-proxy/proxy-health")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return healthProbeResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return healthProbeResult{}, fmt.Errorf("proxy health probe: %s returned %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return healthProbeResult{}, fmt.Errorf("proxy health probe: reading body: %w", err)
	}
	var payload struct {
		Status     string `json:"status"`
		Message    string `json:"message"`
		Country    string `json:"country"`
		RegionName string `json:"regionName"`
		City       string `json:"city"`
		Query      string `json:"query"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// Tolerate the Cloudflare-trace format (ip=/loc=) as a fallback so
		// the checker also works against a plain egress ProbeURL; region and
		// city stay empty in that case.
		if ip, loc := parseTraceBody(body); ip != "" || loc != "" {
			return healthProbeResult{IP: ip, Country: loc}, nil
		}
		return healthProbeResult{}, fmt.Errorf("proxy health probe: unparseable body: %w", err)
	}
	if strings.EqualFold(payload.Status, "fail") || strings.EqualFold(payload.Status, "error") {
		msg := payload.Message
		if msg == "" {
			msg = "probe returned failed status"
		}
		return healthProbeResult{}, errors.New(msg)
	}
	return healthProbeResult{IP: payload.Query, Country: payload.Country, Region: payload.RegionName, City: payload.City}, nil
}

// parseTraceBody extracts ip= and loc= lines from a Cloudflare trace body
// (the shared format with egress.Probe).
func parseTraceBody(body []byte) (ip, loc string) {
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "ip="):
			ip = strings.TrimPrefix(line, "ip=")
		case strings.HasPrefix(line, "loc="):
			loc = strings.TrimPrefix(line, "loc=")
		}
	}
	return ip, loc
}

// healthTarget is one proxy endpoint to probe: normalized addr, the
// redacted configured URL, and its SOCKS5 dialer.
type healthTarget struct {
	addr   string
	raw    string
	dialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// registerTargets registers + validates proxies into h and returns the
// probeable targets (deduplicated; invalid entries skipped with a warning).
func registerTargets(logger *slog.Logger, h *ProxyHealth, proxies []string) []healthTarget {
	targets := make([]healthTarget, 0, len(proxies))
	for _, raw := range proxies {
		addr, added := h.Add(raw)
		if !added {
			if addr != "" {
				logger.Debug("egress health: duplicate proxy skipped", "addr", addr)
			}
			continue
		}
		dialer, err := Socks5Dialer(raw)
		if err != nil {
			logger.Warn("egress health: skipping invalid SOCKS5 proxy", "addr", addr, "err", err)
			continue
		}
		targets = append(targets, healthTarget{addr: addr, raw: redactProxyURL(strings.TrimSpace(raw)), dialer: dialer})
	}
	return targets
}

// checkRound probes every target once with bounded concurrency, recording
// successes/failures into h. Bounded: every probe completes within timeout
// and at most concurrency run simultaneously.
func checkRound(ctx context.Context, logger *slog.Logger, h *ProxyHealth, targets []healthTarget, timeout time.Duration, concurrency int) {
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, tgt := range targets {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(tgt healthTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			res, err := probeHealth(ctx, tgt.dialer, HealthProbeURL, timeout)
			if err != nil {
				h.RecordFailure(tgt.addr, err)
				logger.Warn("egress health probe failed", "proxy", tgt.raw, "err", err)
				return
			}
			latency := time.Since(start).Milliseconds()
			if latency < 1 {
				latency = 1
			}
			h.RecordSuccess(tgt.addr, latency, res.IP, res.Country, res.Region, res.City)
			logger.Debug("egress health probe", "proxy", tgt.raw, "ip", res.IP, "country", res.Country,
				"latency_ms", latency)
		}(tgt)
	}
	wg.Wait()
}

// CheckOnce runs ONE health-check round over proxies (registering and
// deduplicating them), recording the results into h, and returns the
// resulting snapshot. Used by the doctor for a one-shot proxy health
// readout; RunHealthLoop calls the same machinery on its interval.
func (h *ProxyHealth) CheckOnce(ctx context.Context, logger *slog.Logger, proxies []string, timeout time.Duration, concurrency int) []ProxyHealthRecord {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	if concurrency <= 0 {
		concurrency = DefaultHealthConcurrency
	}
	targets := registerTargets(logger, h, proxies)
	if len(targets) > 0 {
		checkRound(ctx, logger, h, targets, timeout, concurrency)
	}
	return h.Snapshot()
}

// RunHealthLoop registers proxies, probes them all immediately, then every
// interval until ctx is canceled, recording successes/failures into h.
// Probes run with bounded concurrency (concurrency <= 0 falls back to
// DefaultHealthConcurrency); interval/timeout <= 0 fall back to the
// defaults. Invalid proxy entries are skipped with a warning (fail-open).
// The loop never leaks goroutines: every probe completes within its timeout
// and the ticker is stopped on ctx cancel.
func RunHealthLoop(ctx context.Context, logger *slog.Logger, h *ProxyHealth, proxies []string, interval, timeout time.Duration, concurrency int) {
	if h == nil {
		panic("egress: RunHealthLoop requires a non-nil ProxyHealth")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultHealthInterval
	}
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	if concurrency <= 0 {
		concurrency = DefaultHealthConcurrency
	}

	targets := registerTargets(logger, h, proxies)
	if len(targets) == 0 {
		logger.Debug("egress health: no healthy proxies to probe")
		return
	}

	checkRound(ctx, logger, h, targets, timeout, concurrency)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkRound(ctx, logger, h, targets, timeout, concurrency)
		}
	}
}
