package phasetiming

import (
	"context"
	"testing"
	"time"
)

func TestSinceAndAll(t *testing.T) {
	p := New()
	p.Since(AcquireMS, time.Now().Add(-10*time.Millisecond))
	p.Since(UpstreamTTFBMS, time.Now().Add(-5*time.Millisecond))
	got := p.All()
	if len(got) != 2 {
		t.Fatalf("All() = %v, want 2 phases", got)
	}
	if got[AcquireMS] < 5 || got[AcquireMS] > 10_000 {
		t.Errorf("AcquireMS = %d ms, want ~10", got[AcquireMS])
	}
	if got[UpstreamTTFBMS] < 1 || got[UpstreamTTFBMS] > 10_000 {
		t.Errorf("UpstreamTTFBMS = %d ms, want ~5", got[UpstreamTTFBMS])
	}
}

func TestContextCarrying(t *testing.T) {
	ctx := context.Background()
	ctx, p := WithContext(ctx)
	if FromContext(ctx) != p {
		t.Fatal("FromContext did not return the installed accumulator")
	}
	if FromContext(context.Background()) != nil {
		t.Fatal("FromContext on a plain context must return nil")
	}
}

func TestNilSafety(t *testing.T) {
	var p *Phases
	p.Since("x", time.Now())
	if p.All() != nil {
		t.Fatal("All on nil must return nil")
	}
	if !p.Start().IsZero() {
		t.Fatal("Start on nil must be zero")
	}
}

func TestAllReturnsCopy(t *testing.T) {
	p := New()
	p.Since("a", time.Now().Add(-time.Millisecond))
	all := p.All()
	all["a"] = 999
	if p.All()["a"] == 999 {
		t.Fatal("All() must return a copy")
	}
}
