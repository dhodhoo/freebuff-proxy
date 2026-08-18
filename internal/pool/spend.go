package pool

// Local per-token spend ledger (issue #87): an in-memory rolling 24h token
// spend window plus a Pacific-midnight day bucket (issue #122) and UTC
// week/month buckets with rollover, mirroring the reference account quota
// bookkeeping (reference/freebuff-reverse internal/accounts/record.go
// QuotaUsed/QuotaPeriodStart and internal/quota/quota.go
// BucketStart/NeedsRollover). Updated from chat usage (pool.RecordSpend,
// fed by the server's parsed usage blocks) and surfaced next to Messages24h
// in the healthz token snapshot.
//
// The account's $15/$5/$0.50 daily spend ceilings (reference/freebuff
// freebuff-spend-ceilings.ts) are SERVER-enforced at Pacific midnight, and
// the proxy cannot know which cohort (full/limited/restricted) a token's
// account sits in — so this ledger is a token-count heuristic, not an exact
// USD accounting. Per-token granularity is the right level: one proxy token
// is one upstream account, so the per-token ledger tracks the same
// per-account spend the server counts.

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
	// Pacific-midnight day bucket (#122) plus UTC week/month buckets, each
	// with their period start (unix); roll over per BucketStart/NeedsRollover
	// semantics.
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

// bucketStart is the start of the period containing now: the day bucket
// rolls at Pacific midnight (America/Los_Angeles), matching the CLI's
// per-account daily ceilings reset "since midnight Pacific"
// (reference/freebuff freebuff-spend-ceilings.ts:5-8 and zoned-time.ts
// getZonedDayBounds) — UTC midnight would split one Pacific day across two
// proxy buckets and misreport the ceiling window (#122). Week stays UTC
// Monday and month UTC 1st (the CLI only models daily ceilings).
func bucketStart(now time.Time, period string) int64 {
	switch period {
	case "week":
		u := now.UTC()
		weekday := int(u.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).
			AddDate(0, 0, -(weekday - 1)).Unix()
	case "month":
		u := now.UTC()
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	default: // day
		return pacificDayStart(now).Unix()
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
		// The day window ends at the NEXT Pacific midnight, which is
		// 23/24/25 hours after the start on DST transition days — a fixed
		// 24h window would roll the bucket at the wrong instant (#122).
		end = nextPacificMidnight(end)
	}
	return !now.Before(end)
}

// pacificDayStart is the Pacific-midnight (America/Los_Angeles) start of the
// day containing now. Prefers the system tz database; when unavailable it
// derives the boundary from the US DST rule (PDT = UTC-7 from the second
// Sunday of March 09:00Z to the first Sunday of November 08:00Z; PST = UTC-8
// otherwise) — never a fixed UTC hour, so the 07:00Z/08:00Z midnight is
// DST-aware (#122, reference/freebuff zoned-time.ts getZonedDayBounds).
func pacificDayStart(now time.Time) time.Time {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		y, m, d := now.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	// No system tzdata: compute the Pacific calendar date of now via the
	// offset in effect at this instant, then that date's midnight via the
	// offset in effect at 00:00 local (the transition Sundays keep the OLD
	// offset at their own midnight — the spring-forward day starts in PST
	// and the fall-back day starts in PDT).
	u := now.UTC()
	local := u.Add(-pacificOffsetAt(u))
	y, m, d := local.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(pacificOffsetAtLocalMidnight(y, m, d))
}

// nextPacificMidnight returns the first Pacific midnight strictly after the
// Pacific-midnight instant start (23/24/25 hours later — the DST day
// length). Wall-clock calendar math, so the offset is re-resolved for the
// new date instead of naively adding 24h.
func nextPacificMidnight(start time.Time) time.Time {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return start.In(loc).AddDate(0, 0, 1)
	}
	// tzdata-less: derive start's Pacific wall-clock date, add one day, and
	// resolve that date's midnight with the offset in effect at 00:00 local.
	u := start.UTC()
	local := u.Add(-pacificOffsetAt(u))
	y, m, d := local.AddDate(0, 0, 1).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(pacificOffsetAtLocalMidnight(y, m, d))
}

// pacificOffsetAt returns the Pacific UTC offset in effect at the instant t:
// PDT (UTC-7) from the second Sunday of March 09:00Z (02:00 PST) to the
// first Sunday of November 08:00Z (02:00 PDT), PST (UTC-8) otherwise.
func pacificOffsetAt(t time.Time) time.Duration {
	u := t.UTC()
	spring := nthSunday(u.Year(), time.March, 2).Add(9 * time.Hour)
	fall := nthSunday(u.Year(), time.November, 1).Add(8 * time.Hour)
	if !u.Before(spring) && u.Before(fall) {
		return 7 * time.Hour
	}
	return 8 * time.Hour
}

// pacificOffsetAtLocalMidnight returns the Pacific UTC offset in effect at
// 00:00 local on the calendar date y-m-d. The transition Sundays keep the
// OLD offset at their own midnight (both transitions happen at 02:00 local),
// so the date is compared strictly inside the DST span.
func pacificOffsetAtLocalMidnight(y int, m time.Month, d int) time.Duration {
	spring := nthSunday(y, time.March, 2)
	fall := nthSunday(y, time.November, 1)
	date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if date.After(spring) && date.Before(fall.AddDate(0, 0, 1)) {
		return 7 * time.Hour
	}
	return 8 * time.Hour
}

// nthSunday returns the calendar date of the nth Sunday in month m of year
// y (US DST rule: second Sunday of March, first Sunday of November).
func nthSunday(y int, m time.Month, n int) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, 0, (7-int(first.Weekday()))%7+(n-1)*7)
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
