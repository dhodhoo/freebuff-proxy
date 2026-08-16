package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestManager(t *testing.T, mock *testutil.MockUpstream) *Manager {
	t.Helper()
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(client)
}

func TestCreateActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Second call is served from cache.
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want still 1", mock.SessionCreates)
	}
}

func TestWaitingRoomThenActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.EstimatedWaitMs = 400
	mock.QueuePosition = 2
	mock.QueueDepth = 5
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want WaitingRoomError, got %v", err)
	}
	if wr.Position != 2 || wr.QueueDepth != 5 {
		t.Errorf("waiting room position/depth = %d/%d", wr.Position, wr.QueueDepth)
	}
	if wr.RetryAfter <= 0 || wr.RetryAfter > time.Second {
		t.Errorf("RetryAfter = %s, want ~400ms", wr.RetryAfter)
	}

	time.Sleep(500 * time.Millisecond)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("polls = %d, want 1", mock.SessionPolls)
	}
}

func TestDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "disabled"
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "" {
		t.Errorf("instance = %q, want empty for disabled", instance)
	}
}

func TestEndedRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"ended", "active"}
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (ended → recreate)", mock.SessionCreates)
	}
}

func TestExpiredCacheRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ExpiresIn = -1 * time.Minute // already past expiry margin
	mgr := newTestManager(t, mock)

	// First call: no cache → one create, state trusted on return.
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Second call: stale cache → refresh (create #2).
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (stale cache → refresh)", mock.SessionCreates)
	}
}

// TestSingleFlightFailureBounded verifies a failed refresh is NOT amplified:
// N concurrent callers must trigger exactly 1 upstream create and all N must
// surface the retained refresh error (instead of each becoming the next
// refresher and re-running the failing POST).
func TestSingleFlightFailureBounded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true // every route returns 429 rate_limited
	mgr := newTestManager(t, mock)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = mgr.EnsureSession(context.Background())
		}(i)
	}
	wg.Wait()

	if mock.Requests != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (single-flight failure must not amplify)", mock.Requests)
	}
	for i, err := range errs {
		if err == nil {
			t.Errorf("caller %d got nil error, want the retained refresh error", i)
			continue
		}
		var rle *upstream.RateLimitError
		if !errors.As(err, &rle) {
			t.Errorf("caller %d error = %T %v, want RateLimitError", i, err, err)
		}
	}
}

// TestPoll404Recreates verifies a poll 404 is treated as ended (recreate
// path) rather than a cached permanent "disabled": the session manager must
// re-create the session after the upstream reports it gone.
func TestPoll404Recreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "404"}
	mock.EstimatedWaitMs = 100
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want WaitingRoomError from queued create, got %v", err)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Wait for pollAt (queued minimum wait is 1s), then poll → 404 → ended →
	// recreate.
	time.Sleep(1100 * time.Millisecond)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (recreated after poll 404)", instance)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("polls = %d, want 1", mock.SessionPolls)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (poll 404 → recreate)", mock.SessionCreates)
	}
}

func TestSingleFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatDelay = 150 * time.Millisecond // slow create
	mgr := newTestManager(t, mock)

	const n = 10
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = mgr.EnsureSession(context.Background())
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != "inst-abc-123" {
			t.Errorf("caller %d instance = %q", i, results[i])
		}
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1 (single-flight)", mock.SessionCreates)
	}
}

func TestConcurrentQueuedSharedState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.EstimatedWaitMs = 100
	mgr := newTestManager(t, mock)

	var wg sync.WaitGroup
	waitRooms := make([]bool, 8)
	instances := make([]string, 8)
	for i := range waitRooms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			instance, err := mgr.EnsureSession(context.Background())
			if err == nil {
				instances[i] = instance
			} else {
				var wr *WaitingRoomError
				waitRooms[i] = errors.As(err, &wr)
			}
		}(i)
	}
	wg.Wait()

	// All callers must either get the waiting room error or the instance;
	// the shared state must not race (exercised under -race).
	gotInstance := false
	for i := range waitRooms {
		if !waitRooms[i] && instances[i] == "" {
			t.Errorf("caller %d: neither waiting room nor instance", i)
		}
		if instances[i] != "" {
			gotInstance = true
		}
	}
	if !gotInstance {
		// All queued is legal; but then no one may hold garbage.
		t.Log("all callers observed the waiting room")
	}
}

func TestInvalidateRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.Invalidate()
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 after Invalidate", mock.SessionCreates)
	}
}

func TestEndSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("ends = %d, want 1", mock.SessionEnds)
	}
	// Cache cleared: next ensure re-creates.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 after EndSession", mock.SessionCreates)
	}
}

func TestCtxCancelPropagates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionCreateDelay = 2 * time.Second
	mgr := newTestManager(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := mgr.EnsureSession(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestBannedSessionReturnsError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "banned"
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want error for banned session")
	}
	if !strings.Contains(err.Error(), "banned") {
		t.Errorf("error = %q, want banned message", err)
	}
}

// TestCountryBlockedSessionReturnsTypedError verifies a country_blocked
// admission surfaces as a CountryBlockedError with the parsed region fields
// (the pre-change code returned a plain fmt.Errorf).
func TestCountryBlockedSessionReturnsTypedError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"US","countryBlockReason":"Free mode is not available in your country","ipPrivacySignals":["vpn"]}`)
	}
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var cbe *upstream.CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
	}
	if cbe.CountryCode != "US" || cbe.CountryBlockReason != "Free mode is not available in your country" {
		t.Errorf("country block fields = %+v", cbe)
	}
	if len(cbe.IpPrivacySignals) != 1 || cbe.IpPrivacySignals[0] != "vpn" {
		t.Errorf("ipPrivacySignals = %v", cbe.IpPrivacySignals)
	}
	if !errors.Is(err, upstream.ErrCountryBlocked) {
		t.Error("not unwrap-able to ErrCountryBlocked")
	}
}

func TestModelLockedRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"model_locked", "active"}
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (model_locked → recreate)", mock.SessionCreates)
	}
}
func TestModelUnavailableFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var createdModels []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			model := r.Header.Get("x-freebuff-model")
			createdModels = append(createdModels, model)
			w.Header().Set("Content-Type", "application/json")
			if model == "rare/model" {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"status":"model_unavailable","requestedModel":"rare/model"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-fallback","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}

	mgr := newTestManager(t, mock)
	instance, err := mgr.EnsureSessionForModel(context.Background(), "rare/model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance != "inst-fallback" {
		t.Errorf("instance = %q, want inst-fallback", instance)
	}
	if len(createdModels) != 2 {
		t.Fatalf("createdModels = %v, want 2 attempts", createdModels)
	}
	if createdModels[0] != "rare/model" || createdModels[1] != "deepseek/deepseek-v4-flash" {
		t.Errorf("createdModels = %v, want rare/model then fallback", createdModels)
	}
}

func TestRateLimitedError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":45000,"limit":5,"recentCount":5}`)
	}

	mgr := newTestManager(t, mock)
	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want error on rate limited session")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want RateLimitError, got %T: %v", err, err)
	}
	if rle.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", rle.RetryAfter)
	}
}

func TestSnapshotModelAndExpiresAt(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}

	snap := mgr.Snapshot()
	if snap.Status != "active" {
		t.Errorf("Status = %q, want active", snap.Status)
	}
	if snap.Model != "thudm/glm-5.2" {
		t.Errorf("Model = %q, want thudm/glm-5.2", snap.Model)
	}
	if snap.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should not be zero")
	}
	// GracePeriodEndsAt (write-only cache field) is surfaced in the snapshot.
	if snap.GracePeriodEndsAt.IsZero() {
		t.Error("GracePeriodEndsAt should not be zero")
	}
	if want := snap.ExpiresAt.Add(graceWindow); !snap.GracePeriodEndsAt.Equal(want) {
		t.Errorf("GracePeriodEndsAt = %v, want %v (expiresAt + graceWindow)", snap.GracePeriodEndsAt, want)
	}
}

func TestSnapshotQuotaByModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
	}
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := mgr.Snapshot()
	q, ok := snap.QuotaByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("QuotaByModel missing z-ai/glm-5.2: %+v", snap.QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not surfaced")
	}
	if q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want referral=1 streak=3", q.Entitlement)
	}
	if len(snap.Entitlement) != 0 {
		t.Errorf("top-level Entitlement = %+v, want empty (nested per model)", snap.Entitlement)
	}
}

func TestHeartbeat(t *testing.T) {
	t.Run("inactive session returns nil", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if err := mgr.Heartbeat(context.Background()); err != nil {
			t.Fatalf("Heartbeat inactive: %v", err)
		}
	})

	t.Run("active session sends heartbeat header", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		_, err := mgr.EnsureSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		heartbeatSeen := false
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("x-freebuff-heartbeat") == "1" {
				heartbeatSeen = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123"}`)
		}

		if err := mgr.Heartbeat(context.Background()); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if !heartbeatSeen {
			t.Error("x-freebuff-heartbeat header not sent upstream")
		}
		if snap := mgr.Snapshot(); snap.Status != "active" {
			t.Errorf("status = %q, want active", snap.Status)
		}
	})

	t.Run("ended session status invalidates state", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		_, err := mgr.EnsureSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123"}`)
		}

		if err := mgr.Heartbeat(context.Background()); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if snap := mgr.Snapshot(); snap.Status != "" {
			t.Errorf("status = %q, want empty after invalidation", snap.Status)
		}
	})
}

// TestHeartbeatStatusErrors verifies Heartbeat maps terminal poll statuses
// through the same statusError helper as refresh, so the pool sees typed
// errors and can cool tokens down.
func TestHeartbeatStatusErrors(t *testing.T) {
	t.Run("banned returns BanError and clears cached admission", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"banned","resumes_at":"2026-08-16T12:00:00Z"}`)
		}

		err := mgr.Heartbeat(context.Background())
		var be *upstream.BanError
		if !errors.As(err, &be) {
			t.Fatalf("want *upstream.BanError, got %v", err)
		}
		if !errors.Is(err, upstream.ErrBanned) {
			t.Error("not unwrap-able to ErrBanned")
		}
		if be.ResumesAt.IsZero() {
			t.Error("resumes_at not parsed into BanError")
		}
		if snap := mgr.Snapshot(); snap.Status != "" {
			t.Errorf("status = %q, want cleared after ban cooldown", snap.Status)
		}
	})

	t.Run("country_blocked returns CountryBlockedError", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"US","countryBlockReason":"region restricted","ipPrivacySignals":["proxy"]}`)
		}

		err := mgr.Heartbeat(context.Background())
		var cbe *upstream.CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
		}
		if cbe.CountryCode != "US" || cbe.CountryBlockReason != "region restricted" {
			t.Errorf("country block fields = %+v", cbe)
		}
		if !errors.Is(err, upstream.ErrCountryBlocked) {
			t.Error("not unwrap-able to ErrCountryBlocked")
		}
	})

	t.Run("rate_limited returns RateLimitError", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":45000,"limit":5,"recentCount":5}`)
		}

		err := mgr.Heartbeat(context.Background())
		var rle *upstream.RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("want *upstream.RateLimitError, got %v", err)
		}
		if !errors.Is(err, upstream.ErrRateLimited) {
			t.Error("not unwrap-able to ErrRateLimited")
		}
		if rle.RetryAfter != 45*time.Second {
			t.Errorf("RetryAfter = %s, want 45s", rle.RetryAfter)
		}
	})
}
