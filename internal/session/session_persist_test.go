package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newPersistTestManager builds a manager wired to a store, like
// newTestManagerWithStore (store_test.go), but returns the store key too so
// tests can seed/assert the persisted slot. The key is the SHA-256 hash of
// the token ("tok"), derived the same way the manager derives it.
func newPersistTestManager(t *testing.T, mock *testutil.MockUpstream, store *Store) (*Manager, string) {
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
	mgr := NewManagerWithStore(client, store)
	return mgr, client.TokenKey()
}

// activeSlot is a persisted active cachedState that pollPersisted would
// consider resumable: unexpired with the grace window still open.
func activeSlot(instanceID, model string) *cachedState {
	expiry := time.Now().Add(time.Hour)
	return &cachedState{
		status:            "active",
		instanceID:        instanceID,
		model:             model,
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
	}
}

// TestPersistResumePollTransportError verifies a transport failure on the
// resume poll surfaces as a refresh error (single-flight / TRANSIENT_RETRIES
// territory) instead of being swallowed and falling through to a fresh
// create that burns a daily session slot. The persisted slot is also left in
// place: the transport failure did not prove it dead.
func TestPersistResumePollTransportError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)
	store.Save(key, activeSlot("inst-abc-123", ""))

	polls, creates := 0, 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			polls++
			// Transport failure: hang up the connection without a response
			// (verified: the Go transport does not retry, single EOF).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("mock server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack failed: %v", err)
				return
			}
			_ = conn.Close()
		case http.MethodPost:
			creates++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("resume poll transport error must surface, got nil")
	}
	if creates != 0 {
		t.Errorf("creates = %d, want 0 (transport error must not fall through to create)", creates)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1 (persisted slot polled once)", polls)
	}
	if got := store.Load(key); got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after transport error = %+v, want intact inst-abc-123", got)
	}
}

// TestPersistModelMismatchNotAdopted verifies a persisted slot bound to a
// different model is never re-adopted: it is dropped so the refresh falls
// through to a create for the requested model. Both the pre-flight gate
// (persisted model known before the poll) and the post-flight gate (the
// upstream binds the resumed slot to a model) are exercised.
func TestPersistModelMismatchNotAdopted(t *testing.T) {
	t.Run("preflight persisted model mismatch", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", "model/A"))

		var createdModels []string
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				// Not reached: the pre-flight gate rejects before the poll.
				t.Errorf("unexpected resume poll for a model-mismatched slot")
			case http.MethodPost:
				createdModels = append(createdModels, r.Header.Get("x-freebuff-model"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-B","model":"`+r.Header.Get("x-freebuff-model")+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		// Direct gate check: pollPersisted must refuse and drop the slot.
		st, err := mgr.pollPersisted(context.Background(), "model/B")
		if err != nil {
			t.Fatalf("pollPersisted: %v", err)
		}
		if st != nil {
			t.Fatalf("pollPersisted adopted %q for model/B, want nil", st.InstanceID)
		}
		if got := store.Load(key); got != nil {
			t.Fatalf("store after mismatch pollPersisted = %+v, want nil (slot dropped)", got)
		}

		// Integration: the refresh falls through to a create for model/B.
		store.Save(key, activeSlot("inst-A", "model/A"))
		instance, err := mgr.EnsureSessionForModel(context.Background(), "model/B")
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-B" {
			t.Errorf("instance = %q, want inst-B (fresh create for model/B, not inst-A)", instance)
		}
		if len(createdModels) != 1 || createdModels[0] != "model/B" {
			t.Errorf("created models = %v, want [model/B]", createdModels)
		}
	})

	t.Run("postflight upstream model mismatch", func(t *testing.T) {
		// The persisted entry carries no model (e.g. written before model
		// tracking), but the upstream binds the resumed slot to model/A.
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", ""))

		polls := 0
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				polls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
			case http.MethodPost:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-B","model":"model/B","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		instance, err := mgr.EnsureSessionForModel(context.Background(), "model/B")
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-B" {
			t.Errorf("instance = %q, want inst-B (post-flight mismatch → create)", instance)
		}
		if polls != 1 {
			t.Errorf("polls = %d, want 1 (slot polled once, then rejected)", polls)
		}
		// The model/A slot was dropped; the store now holds the model/B session.
		if got := store.Load(key); got == nil || got.instanceID != "inst-B" {
			t.Errorf("store = %+v, want model/B session inst-B", got)
		}
	})
}

// TestPersistModelMatchAdopted verifies a persisted slot whose model matches
// the request (or carries no model for a default-model request) is adopted
// without a fresh create.
func TestPersistModelMatchAdopted(t *testing.T) {
	t.Run("same model", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", "model/A"))

		polls, creates := 0, 0
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				polls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
			case http.MethodPost:
				creates++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-X","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		instance, err := mgr.EnsureSessionForModel(context.Background(), "model/A")
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-A" {
			t.Errorf("instance = %q, want inst-A (adopted, not created)", instance)
		}
		if creates != 0 {
			t.Errorf("creates = %d, want 0", creates)
		}
		if polls != 1 {
			t.Errorf("polls = %d, want 1", polls)
		}
	})

	t.Run("default model adopts any slot", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)
		store.Save(key, activeSlot("inst-A", "model/A"))

		creates := 0
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
			case http.MethodPost:
				creates++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-X","expiresAt":"2030-01-01T00:00:00Z"}`)
			default:
				http.NotFound(w, r)
			}
		}

		instance, err := mgr.EnsureSession(context.Background()) // default model ""
		if err != nil {
			t.Fatal(err)
		}
		if instance != "inst-A" {
			t.Errorf("instance = %q, want inst-A (default-model request adopts)", instance)
		}
		if creates != 0 {
			t.Errorf("creates = %d, want 0", creates)
		}
	})
}

// TestShutdownKeepAliveOnlyWhenResumable verifies Shutdown keeps the upstream
// session alive only for genuinely active + unexpired sessions. Every other
// state falls through to the normal EndSession path (upstream DELETE + store
// removal via the CAS commit).
func TestShutdownKeepAliveOnlyWhenResumable(t *testing.T) {
	t.Run("active unexpired kept", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mock.SessionEnds != 0 {
			t.Errorf("SessionEnds = %d, want 0 (active+unexpired kept for restart)", mock.SessionEnds)
		}
		if got := store.Load(key); got == nil || got.instanceID != "inst-abc-123" {
			t.Errorf("store after Shutdown = %+v, want active inst-abc-123", got)
		}
	})

	t.Run("queued ends upstream and drops entry", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionSequence = []string{"queued"}
		mock.EstimatedWaitMs = 60000 // pollAt far in the future
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)

		if _, err := mgr.EnsureSession(context.Background()); err == nil {
			t.Fatal("want WaitingRoomError for queued session")
		} else {
			var wr *WaitingRoomError
			if !errors.As(err, &wr) {
				t.Fatalf("err = %v, want WaitingRoomError", err)
			}
		}
		if got := store.Load(key); got == nil || got.status != "queued" {
			t.Fatalf("store before Shutdown = %+v, want queued entry", got)
		}

		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mock.SessionEnds != 1 {
			t.Errorf("SessionEnds = %d, want 1 (queued releases the upstream slot)", mock.SessionEnds)
		}
		if got := store.Load(key); got != nil {
			t.Errorf("store after Shutdown = %+v, want nil (entry removed)", got)
		}
	})

	t.Run("expired active ends upstream", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		store := NewStore(filepath.Join(t.TempDir(), "state.json"))
		mgr, key := newPersistTestManager(t, mock, store)

		// Craft an active-but-expired cached state (expiry margin already
		// passed) and persist it the way a live manager would.
		mgr.mu.Lock()
		expired := &cachedState{
			status:     "active",
			instanceID: "inst-expired",
			expiresAt:  time.Now().Add(-expiryMargin - time.Second),
		}
		mgr.commit(expired)
		mgr.mu.Unlock()

		if err := mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mock.SessionEnds != 1 {
			t.Errorf("SessionEnds = %d, want 1 (expired-active is not resumable)", mock.SessionEnds)
		}
		if got := store.Load(key); got != nil {
			t.Errorf("store after Shutdown = %+v, want nil", got)
		}
	})
}

// TestPersistStoreNotConsultedOnLiveRefresh verifies the store is only
// consulted when the manager is fresh (cached == nil). A live model-mismatch
// refresh must create for the requested model without polling the persisted
// slot — even when a compatible slot is present, adopting it would pin the
// old model's session on every refresh. The seeded slot carries no model, so
// any store consultation would be visible as a resume poll (GET).
func TestPersistStoreNotConsultedOnLiveRefresh(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr, key := newPersistTestManager(t, mock, store)

	polls := 0
	var createdModels []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			polls++
			// Compatible with any requested model: adopted whenever the
			// store is consulted.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-store","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodPost:
			createdModels = append(createdModels, r.Header.Get("x-freebuff-model"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-created","model":"`+r.Header.Get("x-freebuff-model")+`","expiresAt":"2030-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}

	// First call: fresh manager → the store is consulted and the persisted
	// slot adopted.
	store.Save(key, activeSlot("inst-store", ""))
	instance, err := mgr.EnsureSessionForModel(context.Background(), "model/A")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-store" {
		t.Errorf("first call instance = %q, want inst-store (fresh manager adopts)", instance)
	}
	if polls != 1 {
		t.Errorf("polls after first call = %d, want 1", polls)
	}

	// Live manager, model mismatch: the store must NOT be consulted even
	// though a compatible slot is present (re-seeded model-less so the
	// pre-flight gate cannot hide a consultation).
	store.Save(key, activeSlot("inst-store", ""))
	instance, err = mgr.EnsureSessionForModel(context.Background(), "model/B")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-created" {
		t.Errorf("mismatch refresh instance = %q, want inst-created (created, not adopted)", instance)
	}
	if polls != 1 {
		t.Errorf("polls after mismatch refresh = %d, want still 1 (store not consulted)", polls)
	}
	if len(createdModels) != 1 || createdModels[0] != "model/B" {
		t.Errorf("created models = %v, want [model/B]", createdModels)
	}
}
