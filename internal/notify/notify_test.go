package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSendPostsPayload verifies the webhook POST carries the JSON event
// contract (#48): event name, token index, model, message, RFC3339
// timestamp.
func TestSendPostsPayload(t *testing.T) {
	var got atomic.Value // Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			got.Store(ev)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(srv.URL, nil)
	s.Send(Event{Event: "token_banned", TokenIndex: 3, Model: "deepseek/deepseek-v4-pro", Message: "banned"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		ev, ok := got.Load().(Event)
		if ok && ev.Event != "" {
			if ev.Event != "token_banned" || ev.TokenIndex != 3 || ev.Model != "deepseek/deepseek-v4-pro" || ev.Message != "banned" {
				t.Fatalf("payload = %+v, want full event contract", ev)
			}
			if _, err := time.Parse(time.RFC3339, ev.Timestamp); err != nil {
				t.Fatalf("timestamp %q not RFC3339: %v", ev.Timestamp, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("webhook POST never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSendThrottle verifies at most one POST per event type per 5m window
// (issue #48 dedupe), while distinct event types each get their own slot.
func TestSendThrottle(t *testing.T) {
	var count atomic.Int64
	var mu sync.Mutex
	var events []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(srv.URL, nil)
	s.Send(Event{Event: "pool_exhausted", TokenIndex: 1, Model: "m"})
	s.Send(Event{Event: "pool_exhausted", TokenIndex: 2, Model: "m"}) // throttled
	s.Send(Event{Event: "token_banned", TokenIndex: 1})               // different type: fires
	deadline := time.Now().Add(3 * time.Second)
	for count.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("webhook POSTs = %d, want 2 (one per event type, 5m throttle)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// The POSTs are fire-and-forget goroutines, so arrival order is
	// nondeterministic — assert set membership, not order.
	byType := make(map[string]Event, len(events))
	for _, ev := range events {
		byType[ev.Event] = ev
	}
	if _, ok := byType["pool_exhausted"]; !ok {
		t.Errorf("events missing pool_exhausted: %+v", events)
	}
	ban, ok := byType["token_banned"]
	if !ok {
		t.Fatalf("events missing token_banned: %+v", events)
	}
	if ban.TokenIndex != 1 || ban.Model != "" {
		t.Errorf("token_banned payload = %+v, want index 1", ban)
	}
	if ban.Timestamp == "" {
		t.Error("timestamp missing from webhook payload")
	}
}

// TestSendDisabledNoOps verifies an empty URL (or nil sender) never POSTs.
func TestSendDisabledNoOps(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	New("", nil).Send(Event{Event: "pool_exhausted"})
	var nilSender *Sender
	nilSender.Send(Event{Event: "pool_exhausted"})
	time.Sleep(50 * time.Millisecond)
	if count.Load() != 0 {
		t.Fatalf("POSTs = %d, want 0 for disabled sender", count.Load())
	}
}
