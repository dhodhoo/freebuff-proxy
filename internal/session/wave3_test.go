package session

// Wave-3 session tests: pre-emptive async re-admit before expiry (#99) and
// the admission probe cache TTL (#60).

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestSession(t *testing.T, mock *testutil.MockUpstream) *Manager {
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

// TestReAdmitTriggersAsyncAndRidesOldSession pins issue #99: with the
// re-admit lead larger than the session's remaining life, EnsureSession
// triggers a background re-admit and rides the OLD instance this request;
// the next request gets the NEW instance once the re-admit lands.
func TestReAdmitTriggersAsyncAndRidesOldSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{"status": "active", "instanceId": "inst-x", "expiresAt": time.Now().Add(30 * time.Minute).Format(time.RFC3339)})
			return
		}
		n := creates.Add(1)
		id := "inst-1"
		if n >= 2 {
			id = "inst-2"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": id,
			"expiresAt":  time.Now().Add(10 * time.Second).Format(time.RFC3339),
		})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute) // 10s remaining << 60s lead

	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "inst-1" {
		t.Fatalf("first instance = %q, want inst-1", first)
	}
	// The re-admit is async: this call rode the old session.
	second, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != "inst-1" {
		t.Fatalf("second (during/after re-admit) = %q, want inst-1 (ride old)", second)
	}
	// The background re-admit created a second session.
	eventually(t, "async re-admit", func() bool { return creates.Load() >= 2 })
	// Once the re-admit landed, the next request gets the new instance.
	eventually(t, "new instance served", func() bool {
		id, err := m.EnsureSession(context.Background())
		return err == nil && id == "inst-2"
	})
}

// TestReAdmitFailureRidesOldSession pins the failure path: when the async
// re-admit fails, a request that parks on the single-flight refresh must
// ride the still-usable old session instead of surfacing the error.
func TestReAdmitFailureRidesOldSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusOK, map[string]any{"status": "active", "instanceId": "inst-1", "expiresAt": time.Now().Add(30 * time.Minute).Format(time.RFC3339)})
			return
		}
		if creates.Add(1) >= 2 {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"status": "rate_limited"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": "inst-1",
			"expiresAt":  time.Now().Add(10 * time.Second).Format(time.RFC3339),
		})
	}
	m := newTestSession(t, mock)
	m.SetReAdmitLead(time.Minute)

	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "inst-1" {
		t.Fatalf("first = %q", first)
	}
	// The second call hits the cached-active branch and triggers the async
	// re-admit (which fails) while riding the old session.
	if got, err := m.EnsureSession(context.Background()); err != nil || got != "inst-1" {
		t.Fatalf("second EnsureSession = %q/%v, want inst-1 ride", got, err)
	}
	eventually(t, "re-admit failure recorded", func() bool { return creates.Load() >= 2 })
	// The old session is still usable: the next request rides it.
	got, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatalf("EnsureSession after failed re-admit = %v, want ride old", err)
	}
	if got != "inst-1" {
		t.Fatalf("instance after failed re-admit = %q, want inst-1", got)
	}
}

// TestReAdmitDisabledByDefault pins that the re-admit lead is opt-in: with
// the default (0) no extra session create happens on a fresh session.
func TestReAdmitDisabledByDefault(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m := newTestSession(t, mock)
	first, err := m.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("no instance")
	}
	time.Sleep(50 * time.Millisecond)
	if mock.SessionCreates != 1 {
		t.Fatalf("session creates = %d, want 1 (no async re-admit)", mock.SessionCreates)
	}
}

// TestHeartbeatSkipsWithinProbeTTL pins issue #60(a): heartbeat GETs within
// the admission probe cache TTL of a successful session response are
// skipped; after the TTL the GET happens.
func TestHeartbeatSkipsWithinProbeTTL(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m := newTestSession(t, mock)
	m.SetAdmissionProbeTTL(time.Hour)

	if _, err := m.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	pollsAfterAdmit := mock.SessionPolls
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionPolls != pollsAfterAdmit {
		t.Errorf("heartbeat within probe TTL issued a GET (polls %d → %d)", pollsAfterAdmit, mock.SessionPolls)
	}
}

// TestHeartbeatAfterProbeTTLPolls pins the other side: once the TTL
// elapses, the heartbeat GET fires.
func TestHeartbeatAfterProbeTTLPolls(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m := newTestSession(t, mock)
	m.SetAdmissionProbeTTL(30 * time.Millisecond)

	if _, err := m.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // age past the TTL
	polls := mock.SessionPolls
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionPolls != polls+1 {
		t.Errorf("heartbeat after TTL = %d polls, want %d+1", mock.SessionPolls, polls)
	}
}

// writeJSON renders a JSON response (local helper; testutil's is
// unexported).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
