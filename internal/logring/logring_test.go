package logring

import (
	"context"
	"log/slog"
	"testing"
)

// discarding is a sink that accepts everything and keeps nothing.
type discarding struct{}

func (discarding) Enabled(context.Context, slog.Level) bool  { return true }
func (discarding) Handle(context.Context, slog.Record) error { return nil }
func (d discarding) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discarding) WithGroup(string) slog.Handler           { return d }

func TestRingRetainsNewestFirst(t *testing.T) {
	h := NewHandler(discarding{}, 3)
	logger := slog.New(h)
	for i := range 5 {
		logger.Info("msg", "n", i)
	}
	recent := h.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("Recent(10) = %d entries, want 3 (bounded capacity)", len(recent))
	}
	// Newest first: n=4, n=3, n=2.
	for i, want := range []string{"n=4", "n=3", "n=2"} {
		if recent[i].Message != "msg" {
			t.Fatalf("entry %d message = %q", i, recent[i].Message)
		}
		found := false
		for _, f := range recent[i].Fields {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("entry %d fields %v missing %q", i, recent[i].Fields, want)
		}
	}
}

func TestRingSubHandlersShareStore(t *testing.T) {
	h := NewHandler(discarding{}, 10)
	logger := slog.New(h)
	sub := logger.With("scope", "pool")
	sub.Info("from sub")
	recent := h.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent(1) = %d entries, want 1", len(recent))
	}
	if recent[0].Message != "from sub" {
		t.Errorf("message = %q, want %q", recent[0].Message, "from sub")
	}
	found := false
	for _, f := range recent[0].Fields {
		if f == "scope=pool" {
			found = true
		}
	}
	if !found {
		t.Errorf("bound attr not retained: %v", recent[0].Fields)
	}
}

func TestRingForwardsToNext(t *testing.T) {
	var got string
	next := slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		got += string(p)
		return len(p), nil
	}), nil)
	h := NewHandler(next, 5)
	slog.New(h).Info("hello")
	if got == "" {
		t.Error("record was not forwarded to the wrapped handler")
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
