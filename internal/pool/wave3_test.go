package pool

// Wave-3 pool tests: quota-aware token ordering (#85), the session-create
// admission gate (#86), the local spend ledger (#87), run pre-create at
// admission (#90a), abandoned-lease finish (#53), and step recording (#91).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// futureReset is a ResetAt ~1h out for quota fixtures.
func futureReset() time.Time { return time.Now().Add(time.Hour) }

func quotaFor(model string, limit, recent float64, reset time.Time) map[string]any {
	return map[string]any{
		model: map[string]any{
			"model":       model,
			"limit":       limit,
			"recentCount": recent,
			"period":      "pacific_day",
			"resetAt":     reset.UTC().Format(time.RFC3339),
		},
	}
}

// admitBoth admits sessions for token 0 and 1 on modelA so both are "hot".
func admitBoth(t *testing.T, p *Pool, model string) {
	t.Helper()
	toks := p.toks.Load()
	ctx := context.Background()
	for i := 0; i < len(*toks); i++ {
		if _, err := (*toks)[i].session.EnsureSessionForModel(ctx, model); err != nil {
			t.Fatalf("admit token %d: %v", i, err)
		}
	}
}

func TestAcquireQuotaAwareOrdering(t *testing.T) {
	reset := futureReset()
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 10, 8, reset) // remaining 2
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.RateLimitsByModel = quotaFor(modelA, 10, 2, reset) // remaining 8
	p := newTestPool(t, mock0, mock1)
	admitBoth(t, p, modelA)

	// Both tokens hot with KNOWN positive remaining quota: smallest
	// remaining (token 0, rem 2) must be tried first.
	toks := p.toks.Load()
	order, limited := p.acquireOrder(toks, 0, modelA)
	if len(limited) != 0 {
		t.Fatalf("unexpected quota-limited errors: %v", limited)
	}
	if len(order) < 2 || order[0] != 0 {
		t.Fatalf("order = %v, want token 0 (smallest remaining) first", order)
	}
	// Token 1 must follow (larger remaining) before any cold token.
	if order[1] != 1 {
		t.Errorf("order[1] = %d, want 1", order[1])
	}
}

func TestAcquireKnownQuotaBeforeUnknown(t *testing.T) {
	reset := futureReset()
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 10, 8, reset) // known, rem 2
	mock1 := testutil.NewMock()
	defer mock1.Close() // no quota → unknown
	p := newTestPool(t, mock0, mock1)
	admitBoth(t, p, modelA)

	toks := p.toks.Load()
	order, _ := p.acquireOrder(toks, 0, modelA)
	if len(order) < 2 || order[0] != 0 {
		t.Fatalf("order = %v, want known-quota token 0 first", order)
	}
}

func TestAcquireSkipsQuotaCappedAndSurfaces429(t *testing.T) {
	reset := futureReset()
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 5, 5, reset) // capped
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.RateLimitsByModel = quotaFor(modelA, 5, 5, reset) // capped
	p := newTestPool(t, mock0, mock1)
	admitBoth(t, p, modelA)

	// Both tokens are capped for modelA: Acquire must surface a 429
	// (RateLimitError) with the earliest window reset, not a generic error.
	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("Acquire err = %v, want *RateLimitError", err)
	}
	if rle.Limit != 5 || rle.RecentCount != 5 {
		t.Errorf("rate limit = %g/%g, want 5/5", rle.RecentCount, rle.Limit)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > time.Hour {
		t.Errorf("RetryAfter = %v, want ~1h window", rle.RetryAfter)
	}
	// No session was created for the capped tokens.
	if mock0.SessionCreates != 1 || mock1.SessionCreates != 1 {
		t.Errorf("session creates = %d/%d, want 1/1 (only the admits)", mock0.SessionCreates, mock1.SessionCreates)
	}
}

func TestAcquireStaleQuotaNotCapped(t *testing.T) {
	// RecentCount >= Limit but the window already rolled (past ResetAt):
	// not capped, treated as unknown — never skipped.
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 5, 5, time.Now().Add(-time.Minute))
	p := newTestPool(t, mock0)
	admitBoth(t, p, modelA)

	toks := p.toks.Load()
	order, limited := p.acquireOrder(toks, 0, modelA)
	if len(limited) != 0 {
		t.Fatalf("stale-quota token wrongly capped: %v", limited)
	}
	if len(order) != 1 || order[0] != 0 {
		t.Errorf("order = %v, want [0]", order)
	}
}

func TestCreateGateBlocksAtCapAndReleases(t *testing.T) {
	// Per-model cap: a second acquire on the same model waits until the
	// holder releases (the global cap leaves room, so only the model cap
	// gates it).
	g := newCreateGate(4, 1)
	p1, err := g.acquire(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan *createPermit, 1)
	go func() {
		p, _ := g.acquire(context.Background(), "m1")
		blocked <- p
	}()
	select {
	case <-blocked:
		t.Fatal("per-model cap not enforced")
	case <-time.After(100 * time.Millisecond):
	}
	p1.Release()
	select {
	case got := <-blocked:
		if got == nil {
			t.Fatal("waiter got nil permit")
		}
		got.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken after release")
	}

	// Global cap: with every model cap free, the second acquire still waits
	// until the global holder releases.
	g2 := newCreateGate(1, 4)
	p2, err := g2.acquire(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	blocked2 := make(chan *createPermit, 1)
	go func() {
		p, _ := g2.acquire(context.Background(), "m2")
		blocked2 <- p
	}()
	select {
	case <-blocked2:
		t.Fatal("global cap not enforced")
	case <-time.After(100 * time.Millisecond):
	}
	p2.Release()
	select {
	case got := <-blocked2:
		got.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("global waiter not woken after release")
	}
}

func TestCreateGateWaitExpiresWithCtx(t *testing.T) {
	g := newCreateGate(1, 1)
	p1, err := g.acquire(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = g.acquire(ctx, "m1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire at cap with expiring ctx = %v, want DeadlineExceeded", err)
	}
	p1.Release()
}

func TestAcquireCreateGateWaits(t *testing.T) {
	// Global cap 1: while one admission holds the slot, a second Acquire
	// must wait (its ctx deadline surfaces the wait-or-503 behavior).
	mock0 := testutil.NewMock()
	defer mock0.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SessionCreateMaxParallelGlobal = 1
		c.SessionCreateMaxParallelPerModel = 1
	}, mock0)

	// Hold the gate slot.
	permit, err := p.gate.acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, modelA)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire under gate cap = %v, want DeadlineExceeded (wait-or-503)", err)
	}
	permit.Release()

	// After the release, Acquire succeeds.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
}

func TestSpendLedgerRollover(t *testing.T) {
	p := newTestPool(t, testutil.NewMock())
	p.spendMu.Lock()
	l := p.spendPerToken[0]
	p.spendMu.Unlock()

	now := time.Now()
	l.add(100, now)
	v := ledgerView(l)
	if v.Rolling24h != 100 || v.Day != 100 || v.Week != 100 || v.Month != 100 {
		t.Fatalf("ledger after 100: %+v", v)
	}
	if v.DayStart.IsZero() || v.WeekStart.IsZero() || v.MonthStart.IsZero() {
		t.Fatal("period starts not set")
	}

	// Day rollover: force yesterday's start, add → resets then accumulates.
	l.dayStart = now.Add(-24 * time.Hour).Unix()
	l.add(50, now)
	v = ledgerView(l)
	if v.Day != 50 {
		t.Errorf("Day = %d, want 50 after rollover+add", v.Day)
	}
	if v.Rolling24h != 150 {
		t.Errorf("Rolling24h = %d, want 150", v.Rolling24h)
	}
	if v.Week != 150 {
		t.Errorf("Week = %d, want 150 (week window still open)", v.Week)
	}
}

func TestRecordSpendSurfacesInSnapshot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordSpend(lease, 1234)
	p.LeaseRelease(lease)

	snaps := p.Snapshot()
	if len(snaps) != 1 || snaps[0].Spend24h != 1234 {
		t.Fatalf("snapshot spend = %+v, want Spend24h 1234", snaps)
	}
	if snaps[0].SpendDay != 1234 || snaps[0].SpendWeek != 1234 || snaps[0].SpendMonth != 1234 {
		t.Errorf("period spend = %d/%d/%d, want 1234 each", snaps[0].SpendDay, snaps[0].SpendWeek, snaps[0].SpendMonth)
	}
}

func TestRecordSpendBridge(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock) // bridge-mode pool (client tokens, no AUTH_TOKENS)

	lease, err := p.AcquireBridge(context.Background(), "client-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordSpend(lease, 99)
	v := p.bridgeSpendSnapshot(lease.Bridge)
	if v.Rolling24h != 99 {
		t.Errorf("bridge spend = %d, want 99", v.Rolling24h)
	}
	p.LeaseRelease(lease)
}

func TestLeaseAbandonFinishesRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	runID := lease.Run.RunID
	p.LeaseAbandon(lease) // client disconnect

	eventually(t, "abandoned run FINISH", func() bool {
		for _, f := range mock.FinishedRunsSnapshot() {
			if f.RunID == runID && f.Status == "completed" {
				return true
			}
		}
		return false
	})
}

func TestPrecreateAtAdmissionStartsRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// First Acquire admits the session AND pre-creates the run; the lease
	// then rides it. The mock must see exactly one START for the agent.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Run.RunID == "" {
		t.Fatal("lease without run")
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 || got[0] != agentA {
		t.Fatalf("started runs = %v, want [%s]", got, agentA)
	}
	p.LeaseRelease(lease)
}

func TestRecordRunStepThroughPool(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordRunStep(lease, "chatcmpl-9")
	eventually(t, "step recorded via pool", func() bool {
		steps := mock.StepsSnapshot()
		return len(steps) == 1 && steps[0].StepNumber == 2 && steps[0].MessageID == "chatcmpl-9"
	})
	p.LeaseRelease(lease)
}

// parentFinished filters the mock's FINISH records to parent (non
// context-pruner) runs: the deferred child-run creation (issue #91)
// FINISHes child runs that pre-#91 tests did not expect.
func parentFinished(mock *testutil.MockUpstream) []testutil.FinishedRun {
	var out []testutil.FinishedRun
	for _, f := range mock.FinishedRunsSnapshot() {
		if strings.HasPrefix(f.RunID, "child-run-") {
			continue
		}
		out = append(out, f)
	}
	return out
}
