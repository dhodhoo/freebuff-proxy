package pool

// Local per-token spend ledger (issue #87): an in-memory rolling 24h token
// spend window plus UTC day/week/month buckets with rollover, mirroring the
// reference account quota bookkeeping (reference/freebuff-reverse
// internal/accounts/record.go QuotaUsed/QuotaPeriodStart and
// internal/quota/quota.go BucketStart/NeedsRollover). Updated from chat
// usage (pool.RecordSpend, fed by the server's parsed usage blocks) and
// surfaced next to Messages24h in the healthz token snapshot.

import (
	"time"
)

// spendEntry is one recorded spend amount at a point in time (rolling 24h
// window).
type spendEntry struct {
	at     time.Time
	tokens int64
}

// spendLedger is one token's spend state. Guarded by Pool.spendMu.
type spendLedger struct {
	// rolling is the 24h window: amounts with timestamps, pruned on access.
	rolling []spendEntry
	// UTC day/week/month buckets with their period start (unix); roll over
	// per BucketStart/NeedsRollover semantics.
	dayUsed    int64
	dayStart   int64
	weekUsed   int64
	weekStart  int64
	monthUsed  int64
	monthStart int64
}

func newSpendLedger() *spendLedger { return &spendLedger{} }

// add records tokens spent now, rolling the period buckets when their
// windows closed (NeedsRollover). Caller holds Pool.spendMu.
func (l *spendLedger) add(tokens int64, now time.Time) {
	if l == nil || tokens <= 0 {
		return
	}
	// Rolling 24h window: prune entries outside the window, append this one.
	cutoff := now.Add(-24 * time.Hour)
	first := 0
	for first < len(l.rolling) && l.rolling[first].at.Before(cutoff) {
		first++
	}
	l.rolling = append(l.rolling[first:], spendEntry{at: now, tokens: tokens})

	// Period buckets with rollover.
	l.dayUsed, l.dayStart = rollBucket(l.dayUsed, l.dayStart, "day", now, tokens)
	l.weekUsed, l.weekStart = rollBucket(l.weekUsed, l.weekStart, "week", now, tokens)
	l.monthUsed, l.monthStart = rollBucket(l.monthUsed, l.monthStart, "month", now, tokens)
}

// rollBucket adds tokens to one period bucket, resetting it first when the
// window rolled over (start == 0 or PeriodEnd passed).
func rollBucket(used, start int64, period string, now time.Time, tokens int64) (int64, int64) {
	if needsRollover(start, period, now) {
		start = bucketStart(now, period)
		used = 0
	}
	return used + tokens, start
}

// bucketStart is the UTC start of the period containing now (mirrors
// reference quota.BucketStart: day = UTC midnight, week = UTC Monday,
// month = UTC 1st).
func bucketStart(now time.Time, period string) int64 {
	u := now.UTC()
	switch period {
	case "week":
		weekday := int(u.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).
			AddDate(0, 0, -(weekday - 1)).Unix()
	case "month":
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	default: // day
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).Unix()
	}
}

// needsRollover reports whether a bucket whose window started at start has
// closed by now (mirrors reference quota.NeedsRollover).
func needsRollover(start int64, period string, now time.Time) bool {
	if start == 0 {
		return true
	}
	end := time.Unix(start, 0).UTC()
	switch period {
	case "week":
		end = end.Add(7 * 24 * time.Hour)
	case "month":
		end = end.AddDate(0, 1, 0)
	default:
		end = end.Add(24 * time.Hour)
	}
	return !now.Before(end)
}

// rolling24h prunes the window and returns the total within the last 24h.
// Caller holds Pool.spendMu.
func (l *spendLedger) rolling24h(now time.Time) int64 {
	if l == nil {
		return 0
	}
	cutoff := now.Add(-24 * time.Hour)
	first := 0
	for first < len(l.rolling) && l.rolling[first].at.Before(cutoff) {
		first++
	}
	l.rolling = l.rolling[first:]
	var total int64
	for _, e := range l.rolling {
		total += e.tokens
	}
	return total
}

// --- pool wiring ---

// recordSpend adds tokens to token's ledger (fixed-token index path).
func (p *Pool) recordSpend(token int, tokens int64) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	if token < 0 || token >= len(p.spendPerToken) {
		return
	}
	p.spendPerToken[token].add(tokens, time.Now())
}

// recordSpendEntry adds tokens to the lease's backing entry's ledger by
// pointer (mirrors recordChatEntry: after a concurrent RemoveLastToken+
// AddToken, the lease's Token index may target a different token).
func (p *Pool) recordSpendEntry(entry *tokenEntry, tokens int64) {
	if entry == nil {
		return
	}
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	for idx, tok := range *p.toks.Load() {
		if tok != entry {
			continue
		}
		if idx < 0 || idx >= len(p.spendPerToken) {
			return
		}
		p.spendPerToken[idx].add(tokens, time.Now())
		return
	}
}

// bridgeRecordSpend adds tokens to a bridge entry's ledger.
func (p *Pool) bridgeRecordSpend(entry *bridgeEntry, tokens int64) {
	if entry == nil {
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	entry.spend.add(tokens, time.Now())
}

// spendView is one ledger's snapshot for healthz (issue #87).
type spendView struct {
	Rolling24h int64
	Day        int64
	DayStart   time.Time
	Week       int64
	WeekStart  time.Time
	Month      int64
	MonthStart time.Time
}

// spendSnapshot returns the fixed-token ledger view (index path).
func (p *Pool) spendSnapshot(token int) spendView {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return spendView{}
	}
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	if token < 0 || token >= len(p.spendPerToken) {
		return spendView{}
	}
	return ledgerView(p.spendPerToken[token])
}

// bridgeSpendSnapshot returns the bridge entry's ledger view.
func (p *Pool) bridgeSpendSnapshot(entry *bridgeEntry) spendView {
	if entry == nil {
		return spendView{}
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return ledgerView(entry.spend)
}

// ledgerView snapshots a ledger under its guard.
func ledgerView(l *spendLedger) spendView {
	if l == nil {
		return spendView{}
	}
	now := time.Now()
	return spendView{
		Rolling24h: l.rolling24h(now),
		Day:        l.dayUsed,
		DayStart:   unixToTime(l.dayStart),
		Week:       l.weekUsed,
		WeekStart:  unixToTime(l.weekStart),
		Month:      l.monthUsed,
		MonthStart: unixToTime(l.monthStart),
	}
}

func unixToTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
