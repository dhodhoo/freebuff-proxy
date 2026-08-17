// Package runs implements the per-agent FreeBuff agent-run lifecycle for a
// single token: lazy START on first use, 6h rotation, FINISH drain, 30-min
// auth cooldown, and a shutdown drain. Port of
// reference/proxy-freebuff/lib/runs.js and freebuff2api-quorinex
// run_manager.go (tokenPool half), adapted to this project's layout: the
// session manager is owned by the caller (pool) and only used here for the
// shutdown EndSession, and the pool — not this package — decides which token
// serves a request.
//
// Concurrency: all run bookkeeping is guarded by the manager mutex; no lock
// is held across upstream calls. Rotation swaps the current run under the
// lock and hands the old one to an async finishIfReady, so concurrent
// acquires are race-safe.
package runs

import (
	"context"
	cryptoRand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// DefaultCooldown is the token cooldown applied on upstream auth rejection
// (PRD §5.3: "401 triggers 30-min token cooldown").
const DefaultCooldown = 30 * time.Minute

// countryBlockCooldown is the token cooldown applied when upstream reports a
// region block (country_blocked): long enough to stop the request hammer
// from re-hitting the blocked admission, short enough to re-probe after the
// client switches egress/VPN.
const countryBlockCooldown = 15 * time.Minute

// shutdownTimeout bounds Shutdown when the caller passes a context without a
// deadline (PRD §5: "10s force deadline").
const shutdownTimeout = 10 * time.Second

// Defaults for the bounded deferred-FINISH queue (issue #90) and the
// draining-runs list bounds (issue #55). Overridable via
// runs.Options (pool wires config knobs RUN_FINISH_QUEUE_SIZE /
// RUN_FINISH_INLINE_TIMEOUT / RUNS_DRAIN_QUEUE_CAP / RUNS_DRAIN_TTL).
const (
	defaultFinishQueueSize     = 64
	defaultInlineFinishTimeout = 250 * time.Millisecond
	defaultDrainQueueCap       = 64
	defaultDrainTTL            = 10 * time.Minute
)

// Options configures a RunManager: the rotation interval, the bounded
// deferred-FINISH worker queue bounds (#90/#55), and the optional
// session-state store for run persistence across restarts (#40). Zero
// values fall back to the package defaults.
type Options struct {
	RotationInterval    time.Duration
	FinishQueueSize     int
	InlineFinishTimeout time.Duration
	DrainQueueCap       int
	DrainTTL            time.Duration
	Store               *session.Store
}

// asyncJobKind discriminates the deferred-side-effect jobs carried by the
// bounded finish queue (issue #90/#91): FINISH a rotated/drained run,
// record a completed chat step, or create the context-pruner child run.
type asyncJobKind uint8

const (
	jobFinish asyncJobKind = iota
	jobStep
	jobChildRun
)

// asyncJob is one unit of deferred upstream work. jobs are processed by a
// single background worker per RunManager; when the bounded queue is full
// the caller runs the job inline bounded by the inline timeout.
type asyncJob struct {
	kind      asyncJobKind
	run       *Run
	messageID string
	startTime time.Time
}

// newTraceSessionID mints a UUIDv4 trace session id from crypto/rand,
// mirroring the CLI's randomUUID per run (run.ts: previousRun?.traceSessionId
// ?? randomUUID). A crypto/rand failure is unrecoverable in practice; fall
// back to a time-seeded hex id rather than panicking mid-run.
func newTraceSessionID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Run is one agent run leased to a caller. Requests counts acquires served
// by this run; it is sent as totalSteps when the run is FINISHed.
type Run struct {
	AgentID   string
	RunID     string
	StartedAt time.Time
	Requests  int
	// TraceSessionID is minted once per run (crypto/rand UUID) and reused
	// across the run's requests as codebuff_metadata["trace_session_id"],
	// exactly like the CLI (run.ts: previousRun?.traceSessionId ??
	// randomUUID; reference/proxy-freebuff lib/runs.js:43-46).
	TraceSessionID string

	inflight  int  // leases outstanding; guarded by the manager mutex
	finishing bool // FINISH in flight; guarded by the manager mutex
	// queued marks a deferred FINISH job already in the bounded queue
	// (issue #90): rotate and Maintain both enqueue draining runs, and the
	// dedupe prevents a failed attempt from being FINISHed twice upstream.
	// Guarded by the manager mutex.
	queued bool
	// drainedAt is when the run was pushed onto the draining list (issue
	// #55): the TTL eviction drops entries stuck draining past DrainTTL.
	// Guarded by the manager mutex.
	drainedAt time.Time
}

// runFlight coordinates single-flight coalescing for concurrent StartRun calls.
type runFlight struct {
	done chan struct{}
	err  error
}

// RunSnapshot is a best-effort view of the manager state for healthz.
type RunSnapshot struct {
	ActiveRuns    int
	CooldownUntil time.Time
	Requests      int
	BanError      *upstream.BanError
	// BannedUntil is the ban window deadline; BanError is only "live"
	// while now < BannedUntil (mirrors BanError()'s time check). The pool
	// gates its ban risk label on it so an expired ban is not sticky.
	BannedUntil time.Time
}

// RunManager owns the current runs (one per agent) plus the draining list
// for a single token.
type RunManager struct {
	client           *upstream.Client
	session          *session.Manager
	rotationInterval time.Duration

	mu            sync.Mutex
	runs          map[string]*Run       // agentID → current run
	starting      map[string]*runFlight // agentID → in-flight start
	draining      []*Run                // rotated runs awaiting FINISH
	cooldownUntil time.Time
	// rateLimit is the last 429 rate-limit error applied to this token's
	// cooldown. It is surfaced by RateLimitError() so exhausted tokens
	// keep returning 429 + Retry-After instead of a generic 502 while the
	// cooldown window is active.
	rateLimit *upstream.RateLimitError
	// banUntil is set when the account is banned; Acquire rejects with the
	// remembered ban error until the unban time.
	banUntil time.Time
	ban      *upstream.BanError
	// countryBlock is the last country-block error applied to this token's
	// cooldown. It is surfaced by CountryBlockedError() so a region-blocked
	// token keeps returning the block error instead of re-hitting upstream
	// during the window (mirrors the rate-limit/ban memory).
	countryBlock *upstream.CountryBlockedError
	countryUntil time.Time
	// ipCapped is the last ip_capped admission refusal applied to this
	// token's cooldown. Surfaced by IpCappedError() during its short window
	// (mirrors rateLimit) so an IP-capped token keeps returning 429
	// ip_capped + Retry-After instead of a generic cooldown 502.
	ipCapped      *upstream.IpCappedError
	ipCappedUntil time.Time
	// totalRequests is the cumulative count of Acquire leases handed out.
	// It is kept separate from the per-run counters because rotated runs
	// that get FINISHed leave the active+draining sets and would otherwise
	// take their request counts out of Snapshot.
	totalRequests int

	// Deferred-FINISH queue (issue #90): rotated/drained runs, chat steps,
	// and child-run creation are processed by one background worker per
	// finishQueue is bounded (Options.FinishQueueSize); when it is
	// full the caller runs the job inline bounded by inlineFinishTimeout.
	// finishStop is closed once (finishOnce) by Shutdown; the worker drains
	// the queue and exits, tracked by finishWg. finishStartOnce starts the
	// worker on first use. finishExited is closed by the worker on exit
	// (test hook for goroutine-leak assertions).
	finishQueue         chan asyncJob
	finishStop          chan struct{}
	finishOnce          sync.Once
	finishStartOnce     sync.Once
	finishWg            sync.WaitGroup
	finishExited        chan struct{}
	inlineFinishTimeout time.Duration
	// drainQueueCap / drainTTL bound the draining list (issue #55).
	drainQueueCap int
	drainTTL      time.Duration

	// store persists active runs across restarts (SESSION_PERSIST, issue
	// #40); nil disables. key is the stable token hash
	// (upstream.Client.TokenKey) mirroring the session store's key space.
	store *session.Store
	key   string
}

// NewRunManager builds the manager for one token. rotationInterval is how
// long a run lives before it is rotated (config ROTATION_INTERVAL, default
// 6h). The session manager is used only for Shutdown's EndSession.
func NewRunManager(client *upstream.Client, session *session.Manager, rotationInterval time.Duration) *RunManager {
	return NewRunManagerOpts(client, session, Options{RotationInterval: rotationInterval})
}

// NewRunManagerOpts builds the manager with full Options (rotation
// interval plus the bounded finish queue and draining-list bounds from
// issues #90/#55 and optional run persistence from #40). Zero option
// values fall back to the package defaults.
func NewRunManagerOpts(client *upstream.Client, session *session.Manager, opts Options) *RunManager {
	queueSize := opts.FinishQueueSize
	if queueSize < 1 {
		queueSize = defaultFinishQueueSize
	}
	inlineTimeout := opts.InlineFinishTimeout
	if inlineTimeout <= 0 {
		inlineTimeout = defaultInlineFinishTimeout
	}
	drainCap := opts.DrainQueueCap
	if drainCap < 1 {
		drainCap = defaultDrainQueueCap
	}
	drainTTL := opts.DrainTTL
	if drainTTL <= 0 {
		drainTTL = defaultDrainTTL
	}
	m := &RunManager{
		client:              client,
		session:             session,
		rotationInterval:    opts.RotationInterval,
		runs:                make(map[string]*Run),
		starting:            make(map[string]*runFlight),
		finishQueue:         make(chan asyncJob, queueSize),
		finishStop:          make(chan struct{}),
		finishExited:        make(chan struct{}),
		inlineFinishTimeout: inlineTimeout,
		drainQueueCap:       drainCap,
		drainTTL:            drainTTL,
	}
	if client != nil {
		m.key = client.TokenKey()
	}
	if opts.Store != nil {
		m.store = opts.Store
	}
	return m
}

// SetStore injects the shared session-state store used for run persistence
// (SESSION_PERSIST, issue #40). The pool calls this on SetSessionStore for
// the fixed-token managers (built before the store exists) and passes the
// store through Options for runtime-added tokens. A nil store disables run
// persistence. Runs already tracked keep their state; persistence applies
// to subsequent START/FINISH transitions.
func (m *RunManager) SetStore(store *session.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// Acquire returns the current run for agentID, starting one on first use or
// rotating when the current run has reached the rotation interval. The
// rotated run is pushed to the draining list and FINISHed asynchronously.
// The returned run has its inflight and Requests counters incremented;
// callers must Release it when the request completes or fails.
func (m *RunManager) Acquire(ctx context.Context, agentID string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The re-validation loop converges: an idle FinishAllRuns (or a
	// concurrent Shutdown) may clear the run map between the initial read
	// and the re-read below, which would otherwise surface a phantom
	// "run missing after rotation" failure to the caller. Each pass either
	// returns a lease or re-creates the current run under the manager
	// mutex, so a cleared map is re-populated on the next iteration.
	// FinishAllRuns clears at most once per idle stretch, so production
	// converges in one retry; ctx cancellation bounds the loop.
	for {
		m.mu.Lock()
		if now := time.Now(); now.Before(m.cooldownUntil) {
			until := m.cooldownUntil
			m.mu.Unlock()
			return nil, fmt.Errorf("token cooling down until %s", until.Format(time.RFC3339))
		}
		run := m.runs[agentID]
		needsRotate := run == nil || time.Since(run.StartedAt) >= m.rotationInterval
		m.mu.Unlock()

		if needsRotate {
			if err := m.rotate(ctx, agentID); err != nil {
				return nil, err
			}
		}

		m.mu.Lock()
		// A concurrent acquire may have rotated again while we were
		// starting; the lease must always point at the current run.
		run = m.runs[agentID]
		if run != nil {
			run.inflight++
			run.Requests++
			m.totalRequests++
			m.mu.Unlock()
			return run, nil
		}
		m.mu.Unlock()

		// The current run vanished mid-acquire (concurrent FinishAllRuns);
		// loop and re-validate instead of failing the request.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// Release decrements the inflight counter of a leased run. Safe on nil.
// Draining finishes happen on the maintain tick or the next rotation.
func (m *RunManager) Release(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	m.mu.Unlock()
}

// InflightCount returns the number of outstanding leases across all runs
// (active and draining). The pool uses it to skip evicting bridge entries
// whose run is still serving a request: FINISHing such a run would kill the
// in-flight chat, so those entries are left for the idle sweep instead.
func (m *RunManager) InflightCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, run := range m.runs {
		n += run.inflight
	}
	for _, run := range m.draining {
		n += run.inflight
	}
	return n
}

// FinishRun FINISHes the run upstream with the given step accounting and
// drops it from the active set. On upstream failure the run is put back on
// the draining list so Maintain retries it. It does not touch inflight —
// callers should have already Released the lease.
func (m *RunManager) FinishRun(ctx context.Context, run *Run, totalSteps int) {
	if run == nil {
		return
	}
	m.drop(run)
	if err := m.client.FinishRun(ctx, run.RunID, totalSteps); err != nil {
		// Keep the run around for a Maintain retry; the id is not
		// necessarily dead upstream (network errors, 5xx).
		m.mu.Lock()
		run.drainedAt = time.Now()
		m.draining = append(m.draining, run)
		m.pruneDrainingLocked()
		m.mu.Unlock()
		slog.Debug("runs: FINISH failed, will retry on maintain", "run_id", run.RunID, "err", err)
		return
	}
	// FINISHed cleanly: a restart must not resurrect the run.
	m.removeRun(run)
}

// Maintain rotates aged runs and FINISHes the draining list. Runs with
// outstanding inflight leases or an in-flight FINISH are skipped. Best
// effort: failures are logged, never returned (background job). While the
// token is cooling down (auth rejection, rate limit, ban) the pass returns
// immediately: no rotate attempts, no draining FINISH, no log — retrying
// upstream work during a cooldown looks like abuse and would log the
// "token cooling down" rotate failure once per maintain tick (observed in
// production). The pool logs the skip.
func (m *RunManager) Maintain(ctx context.Context) {
	if time.Now().Before(m.CooldownUntil()) {
		return
	}
	m.mu.Lock()
	var toRotate []string
	for agentID, run := range m.runs {
		if time.Since(run.StartedAt) >= m.rotationInterval {
			toRotate = append(toRotate, agentID)
		}
	}
	// Bound the draining list before re-enqueuing its FINISHes: entries
	// past the TTL or cap are force-dropped (issue #55) so a persistently
	// failing FINISH cannot grow the list without bound.
	m.pruneDrainingLocked()
	draining := append([]*Run(nil), m.draining...)
	m.mu.Unlock()

	for _, agentID := range toRotate {
		if err := m.rotate(ctx, agentID); err != nil {
			slog.Debug("runs: maintain rotate failed", "agent_id", agentID, "err", err)
		}
	}
	// Deferred-FINISH through the bounded queue (issue #90): the maintain
	// tick never blocks on upstream FINISH calls; the worker (or the inline
	// fallback) owns them. finishIfReady skips busy/finishing runs, so a
	// run with an outstanding lease stays draining for the next pass.
	for _, run := range draining {
		m.enqueueFinish(run)
	}
}

// Shutdown FINISHes every run (active and draining) and ends the upstream
// session. When ctx carries no deadline a 10s force deadline is applied
// (PRD §5.5 shutdown sequence).
func (m *RunManager) Shutdown(ctx context.Context) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
	}

	// Stop the deferred-job worker first (issue #90): it drains whatever is
	// queued, so its FINISHes land before our own claim below, and no job
	// can start after we snapshot the runs. The worker's finishing-flag
	// claims are respected by the claim loop (finishing runs are skipped).
	m.finishOnce.Do(func() { close(m.finishStop) })
	m.finishWg.Wait()

	// Run persistence (issue #40): with a store, keep the active runs alive
	// across the restart like the session keep-alive — FINISHing them here
	// would force the next process to re-START and burn upstream calls. The
	// runs are already persisted on START; re-save so the latest Requests
	// counter survives.
	if m.store != nil && m.key != "" {
		m.mu.Lock()
		snapshot := make([]Run, 0, len(m.runs))
		for _, run := range m.runs {
			snapshot = append(snapshot, *run)
		}
		m.mu.Unlock()
		for i := range snapshot {
			m.persistRun(&snapshot[i])
		}
		if err := m.session.Shutdown(ctx); err != nil {
			slog.Warn("runs: shutdown session with errors", "errors", err)
		}
		return
	}

	m.mu.Lock()
	// Skip runs with a FINISH already in flight (an async rotate drain
	// owns them): re-FINISHing the same run id upstream is a duplicate
	// call the drain goroutine is already completing. Claim the rest by
	// setting finishing so a concurrently starting finishIfReady cannot
	// double-FINISH a run we are about to finish here.
	all := make([]*Run, 0, len(m.runs)+len(m.draining))
	for _, run := range m.runs {
		if run.finishing {
			continue
		}
		run.finishing = true
		all = append(all, run)
	}
	for _, run := range m.draining {
		if run.finishing {
			continue
		}
		run.finishing = true
		all = append(all, run)
	}
	m.runs = make(map[string]*Run)
	m.draining = nil
	m.mu.Unlock()

	var errs []string
	for _, run := range all {
		if err := m.client.FinishRun(ctx, run.RunID, run.Requests); err != nil {
			errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
		}
	}
	if err := m.session.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("shutdown session: %v", err))
	}
	if len(errs) > 0 {
		slog.Warn("runs: shutdown with errors", "errors", strings.Join(errs, "; "))
	}
}

// FinishAllRuns FINISHes every active run and drops it from the active set,
// leaving the session untouched (unlike Shutdown). Used by the pool's idle
// rotation: once a token has been idle past IDLE_ROTATION_TIMEOUT, its runs
// are finished so no rotation/refresh activity continues upstream; the next
// Acquire starts a fresh run on demand.
func (m *RunManager) FinishAllRuns(ctx context.Context) {
	m.mu.Lock()
	all := make([]*Run, 0, len(m.runs))
	for _, run := range m.runs {
		all = append(all, run)
	}
	m.runs = make(map[string]*Run)
	m.mu.Unlock()

	var errs []string
	for _, run := range all {
		if err := m.client.FinishRun(ctx, run.RunID, run.Requests); err != nil {
			errs = append(errs, fmt.Sprintf("finish run %s: %v", run.RunID, err))
		} else {
			m.removeRun(run)
		}
	}
	if len(errs) > 0 {
		slog.Warn("runs: idle finish with errors", "errors", strings.Join(errs, "; "))
	}
}

// Invalidate drops the current run for agentID so the next Acquire starts a
// fresh one. Used when an upstream chat reports the run id as unknown
// (ErrRunInvalid); the dead run is not FINISHed (upstream already forgot it)
// and not drained.
func (m *RunManager) Invalidate(agentID string) {
	m.mu.Lock()
	delete(m.runs, agentID)
	m.mu.Unlock()
}

// Cooldown puts the token in a cooldown window of duration d (e.g.
// DefaultCooldown after an auth rejection). Durations <= 0 are ignored.
func (m *RunManager) Cooldown(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.cooldownUntil = time.Now().Add(d)
	m.rateLimit = nil
	m.ban = nil
	m.countryBlock = nil
	m.ipCapped = nil
	// The ban/country windows die with their remembered errors: leaving the
	// deadlines set would surface a stale future BannedUntil (healthz risk
	// gating via Snapshot) with no ban attached. Mirror ClearCooldowns.
	m.banUntil = time.Time{}
	m.countryUntil = time.Time{}
	m.ipCappedUntil = time.Time{}
	m.mu.Unlock()
}

// ClearCooldowns removes any cooldown, rate-limit lock, and ban window so
// the token is immediately acquirable again (dashboard unlock action).
func (m *RunManager) ClearCooldowns() {
	m.mu.Lock()
	m.cooldownUntil = time.Time{}
	m.rateLimit = nil
	m.ban = nil
	m.banUntil = time.Time{}
	m.countryBlock = nil
	m.countryUntil = time.Time{}
	m.ipCapped = nil
	m.ipCappedUntil = time.Time{}
	m.mu.Unlock()
}

// CooldownUntil returns the cooldown deadline (zero when not cooling down).
func (m *RunManager) CooldownUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cooldownUntil
}

// CooldownRateLimit applies a rate-limit cooldown and remembers the error
// so subsequent Acquires surface 429 + Retry-After instead of a generic
// 502. Errors with RetryAfter <= 0 are ignored.
func (m *RunManager) CooldownRateLimit(rle *upstream.RateLimitError) {
	if rle == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rle.RetryAfter > 0 {
		m.cooldownUntil = time.Now().Add(rle.RetryAfter)
	} else if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		m.cooldownUntil = rle.ResetAt
	} else {
		m.cooldownUntil = upstream.NextPacificMidnight()
	}
	m.rateLimit = rle
	m.ban = nil
	m.countryBlock = nil
	m.ipCapped = nil
}

// CooldownIpCapped applies an ip_capped cooldown bounded to the body's
// retryAfterMs ONLY — never the Pacific-midnight quota lock (ip_capped is
// admission-only, not tied to a quota reset). Remembered so Acquires keep
// surfacing 429 ip_capped + Retry-After during the short window instead of
// re-hitting upstream (mirrors CooldownRateLimit). Errors with
// RetryAfter <= 0 are ignored.
func (m *RunManager) CooldownIpCapped(ice *upstream.IpCappedError) {
	if ice == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ice.RetryAfter <= 0 {
		return
	}
	m.ipCapped = ice
	m.ipCappedUntil = time.Now().Add(ice.RetryAfter)
	m.cooldownUntil = m.ipCappedUntil
	m.rateLimit = nil
	m.ban = nil
	m.countryBlock = nil
}

// IpCappedError returns the remembered ip_capped error while its short
// cooldown window is active, nil otherwise.
func (m *RunManager) IpCappedError() *upstream.IpCappedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.ipCappedUntil) && m.ipCapped != nil {
		return m.ipCapped
	}
	return nil
}

// RateLimitError returns the remembered rate-limit error while its
// cooldown is still active, nil otherwise.
func (m *RunManager) RateLimitError() *upstream.RateLimitError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.cooldownUntil) && m.rateLimit != nil {
		return m.rateLimit
	}
	return nil
}

// CooldownBan applies a ban cooldown and remembers the error so Acquires
// keep surfacing 403 banned + resumes-at until the unban time.
func (m *RunManager) CooldownBan(be *upstream.BanError) {
	if be == nil {
		return
	}
	m.mu.Lock()
	m.ban = be
	if be.ResumesAt.After(time.Now()) {
		m.banUntil = be.ResumesAt
	} else {
		m.banUntil = time.Now().Add(24 * time.Hour) // no timestamp: safe default
	}
	// The ban also fills the shared cooldown deadline so Acquire skips the
	// token entirely during the window (the remembered error is surfaced by
	// the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.banUntil
	m.rateLimit = nil // a ban supersedes any rate-limit cooldown
	m.countryBlock = nil
	m.ipCapped = nil
	m.mu.Unlock()
}

// BanError returns the remembered ban error while the ban window is
// active, nil otherwise.
func (m *RunManager) BanError() *upstream.BanError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.banUntil) && m.ban != nil {
		return m.ban
	}
	return nil
}

// CooldownCountryBlocked applies a country-block cooldown and remembers the
// error so Acquires keep surfacing the region-block instead of re-hitting
// upstream during the window (mirrors CooldownRateLimit/CooldownBan).
func (m *RunManager) CooldownCountryBlocked(cbe *upstream.CountryBlockedError) {
	if cbe == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A ban outranks a country block (pool precedence ban > country): keep
	// the ban window and its remembered error instead of downgrading to the
	// shorter country cooldown.
	if time.Now().Before(m.banUntil) && m.ban != nil {
		return
	}
	m.countryBlock = cbe
	m.countryUntil = time.Now().Add(countryBlockCooldown)
	// The block also fills the shared cooldown deadline so Acquire skips
	// the token entirely during the window (the remembered error is
	// surfaced by the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.countryUntil
	m.rateLimit = nil
	m.ban = nil
	m.banUntil = time.Time{}
	m.ipCapped = nil
}

// CountryBlockedError returns the remembered country-block error while its
// cooldown window is active, nil otherwise.
func (m *RunManager) CountryBlockedError() *upstream.CountryBlockedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.countryUntil) && m.countryBlock != nil {
		return m.countryBlock
	}
	return nil
}

// Snapshot returns a best-effort view of the manager state.
func (m *RunManager) Snapshot() RunSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := RunSnapshot{ActiveRuns: len(m.runs), CooldownUntil: m.cooldownUntil, Requests: m.totalRequests, BanError: m.ban, BannedUntil: m.banUntil}
	return s
}

// Prewarm starts a run for every agent that does not already have a fresh
// one, best-effort (per-agent errors are logged, never returned). Used at
// pool boot so the first request does not pay the START latency.
func (m *RunManager) Prewarm(ctx context.Context, agentIDs []string) {
	for _, agentID := range agentIDs {
		m.mu.Lock()
		needs := m.runs[agentID] == nil
		m.mu.Unlock()
		if !needs {
			continue
		}
		if err := m.rotate(ctx, agentID); err != nil {
			slog.Debug("runs: prewarm failed", "agent_id", agentID, "err", err)
		}
	}
}

// --- internals ---

// rotate starts a fresh run for agentID, pushing the previous current run
// (if any) onto the draining list and finishing it asynchronously. Single-flight
// coalescing ensures concurrent callers for the same agent wait on a single
// upstream StartRun call rather than launching duplicate requests.
func (m *RunManager) rotate(ctx context.Context, agentID string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		if now := time.Now(); now.Before(m.cooldownUntil) {
			until := m.cooldownUntil
			m.mu.Unlock()
			return fmt.Errorf("token cooling down until %s", until.Format(time.RFC3339))
		}
		if run := m.runs[agentID]; run != nil && time.Since(run.StartedAt) < m.rotationInterval {
			m.mu.Unlock()
			return nil // a concurrent rotator already refreshed it
		}
		if flight, ok := m.starting[agentID]; ok {
			ch := flight.done
			m.mu.Unlock()
			select {
			case <-ch:
				if flight.err != nil {
					if (errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) && ctx.Err() == nil {
						// Leader goroutine canceled/timed out, but this waiter's context is still active.
						// Loop back to try becoming leader.
						continue
					}
					return flight.err
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// We are the leader for starting this agent's run.
		flight := &runFlight{done: make(chan struct{})}
		if m.starting == nil {
			m.starting = make(map[string]*runFlight)
		}
		m.starting[agentID] = flight
		m.mu.Unlock()

		// Issue #40: resume a persisted run instead of STARTing a fresh one
		// when a restart left an active run behind. Only runs started within
		// the rotation interval are adopted — a stale entry is dropped so
		// the upstream's own rotation wins. Best-effort: the store read
		// never fails the rotate.
		if m.store != nil && m.key != "" {
			if pr := m.store.LoadRun(m.key, agentID); pr != nil {
				if pr.RunID != "" && time.Since(pr.StartedAt) < m.rotationInterval {
					m.mu.Lock()
					oldRun := m.runs[agentID]
					m.runs[agentID] = &Run{
						AgentID:        agentID,
						RunID:          pr.RunID,
						StartedAt:      pr.StartedAt,
						TraceSessionID: pr.TraceSessionID,
						Requests:       pr.Requests,
					}
					flight.err = nil
					close(flight.done)
					delete(m.starting, agentID)
					if oldRun != nil {
						oldRun.drainedAt = time.Now()
						m.draining = append(m.draining, oldRun)
						m.pruneDrainingLocked()
					}
					m.mu.Unlock()
					if oldRun != nil {
						m.enqueueFinish(oldRun)
					}
					slog.Debug("runs: run resumed from store", "agent_id", agentID, "run_id", pr.RunID)
					return nil
				}
				m.store.RemoveRun(m.key, agentID)
			}
		}

		runID, err := m.client.StartRun(ctx, agentID)

		m.mu.Lock()
		flight.err = err
		close(flight.done)
		delete(m.starting, agentID)

		if err != nil {
			m.mu.Unlock()
			return err
		}
		slog.Debug("runs: run started", "agent_id", agentID, "run_id", runID)

		newRun := &Run{AgentID: agentID, RunID: runID, StartedAt: time.Now(), TraceSessionID: newTraceSessionID()}
		oldRun := m.runs[agentID]
		m.runs[agentID] = newRun
		if oldRun != nil {
			oldRun.drainedAt = time.Now()
			m.draining = append(m.draining, oldRun)
			m.pruneDrainingLocked()
		}
		m.mu.Unlock()

		m.persistRun(newRun)
		if oldRun != nil {
			m.enqueueFinish(oldRun)
		}
		// Issue #91: create the context-pruner child of the new parent run
		// (ancestorRunIds=[parent]), best-effort through the bounded queue.
		m.enqueue(asyncJob{kind: jobChildRun, run: newRun})
		return nil
	}
}

// enqueueFinish submits a deferred FINISH for run through the bounded queue
// (issue #90). Runs already queued are skipped (rotate and Maintain both
// enqueue draining runs; without the dedupe a failed attempt would be
// FINISHed twice upstream once the finishing flag resets).
func (m *RunManager) enqueueFinish(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.queued {
		m.mu.Unlock()
		return
	}
	run.queued = true
	m.mu.Unlock()
	m.enqueue(asyncJob{kind: jobFinish, run: run})
}

// enqueue submits a deferred upstream job to the bounded finish queue
// (issue #90). When the queue is full the job runs inline, bounded by the
// inline finish timeout — the caller never blocks on the worker.
func (m *RunManager) enqueue(job asyncJob) {
	if job.run == nil && job.kind != jobChildRun {
		return
	}
	if m.finishQueue == nil {
		return
	}
	m.startFinishWorker()
	select {
	case m.finishQueue <- job:
	default:
		// Queue full: synchronous inline fallback bounded by the short
		// inline deadline (mirrors the reference async finalizer's
		// finalizeInlineTimeout). A run whose FINISH exceeds the deadline is
		// left draining for the next Maintain retry.
		ctx, cancel := context.WithTimeout(context.Background(), m.inlineFinishTimeout)
		defer cancel()
		m.runJob(ctx, job)
	}
}

// startFinishWorker launches the single deferred-job worker on first use.
// The worker exits when Shutdown closes finishStop after draining the
// queue, so no goroutine outlives the manager.
func (m *RunManager) startFinishWorker() {
	m.finishStartOnce.Do(func() {
		m.finishWg.Add(1)
		go m.finishLoop()
	})
}

// finishLoop is the deferred-job worker: FINISH rotated/drained runs,
// record chat steps, and create context-pruner child runs, all best-effort.
func (m *RunManager) finishLoop() {
	defer m.finishWg.Done()
	defer close(m.finishExited)
	for {
		select {
		case <-m.finishStop:
			// Shutdown: drain whatever is queued, then exit.
			for {
				select {
				case job := <-m.finishQueue:
					m.runJob(context.Background(), job)
				default:
					return
				}
			}
		case job := <-m.finishQueue:
			m.runJob(context.Background(), job)
		}
	}
}

// runJob executes one deferred job (FINISH / step / child run). Best-effort:
// failures are logged, never surfaced to a caller.
func (m *RunManager) runJob(ctx context.Context, job asyncJob) {
	switch job.kind {
	case jobFinish:
		m.finishIfReadyCtx(ctx, job.run)
	case jobStep:
		if job.run == nil || job.run.RunID == "" || m.client == nil {
			return
		}
		if err := m.client.RecordRunStep(ctx, job.run.RunID, job.messageID, job.startTime); err != nil {
			slog.Debug("runs: record step failed", "run_id", job.run.RunID, "err", err)
		}
	case jobChildRun:
		m.createChildRun(ctx, job.run)
	}
}

// createChildRun starts the context-pruner child of parentRunID and FINISHes
// it once created (issue #91, CLI parity: createChildRun + finishChildRun).
// Best-effort: failures are logged only.
func (m *RunManager) createChildRun(ctx context.Context, parent *Run) {
	if parent == nil || parent.RunID == "" || m.client == nil {
		return
	}
	childID, err := m.client.StartChildRun(ctx, parent.RunID)
	if err != nil {
		slog.Debug("runs: context-pruner child start failed", "parent_run_id", parent.RunID, "err", err)
		return
	}
	if err := m.client.FinishRun(ctx, childID, 1); err != nil {
		slog.Debug("runs: context-pruner child finish failed", "child_run_id", childID, "err", err)
	}
}

// RecordStep queues a completed-chat step recording for run (issue #91):
// the server fires it after a successful chat with the response message id.
// Best-effort through the bounded queue; failures are logged only.
func (m *RunManager) RecordStep(run *Run, messageID string) {
	if run == nil || run.RunID == "" {
		return
	}
	m.enqueue(asyncJob{kind: jobStep, run: run, messageID: messageID, startTime: time.Now()})
}

// Precreate starts the run for agentID if none is fresh, without leasing it
// (issue #90a): the pool calls it right after a session admission so the
// first chat on a newly-admitted session does not pay the START latency.
// Best-effort: the caller's Acquire surfaces any real failure through the
// normal path.
func (m *RunManager) Precreate(ctx context.Context, agentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	run := m.runs[agentID]
	needs := run == nil || time.Since(run.StartedAt) >= m.rotationInterval
	m.mu.Unlock()
	if !needs {
		return nil
	}
	return m.rotate(ctx, agentID)
}

// persistRun writes the run into the session-state store (issue #40) so a
// restart can resume it without re-START. Best-effort; the store write
// never fails the caller.
func (m *RunManager) persistRun(run *Run) {
	if m.store == nil || m.key == "" || run == nil {
		return
	}
	m.mu.Lock()
	requests := run.Requests
	startedAt := run.StartedAt
	m.mu.Unlock()
	m.store.SaveRun(m.key, run.AgentID, session.PersistedRun{
		RunID:          run.RunID,
		AgentID:        run.AgentID,
		TraceSessionID: run.TraceSessionID,
		StartedAt:      startedAt,
		Requests:       requests,
	})
}

// removeRun drops the run from the session-state store (issue #40): the
// run was FINISHed upstream, so a restart must not resurrect it.
func (m *RunManager) removeRun(run *Run) {
	if m.store == nil || m.key == "" || run == nil {
		return
	}
	m.store.RemoveRun(m.key, run.AgentID)
}

// pruneDrainingLocked bounds the draining list (issue #55): entries stuck
// past DrainTTL or beyond DrainQueueCap are force-dropped with a warn log —
// their upstream FINISH is best-effort anyway, and the list must never grow
// unbounded when FINISH keeps failing. Caller holds m.mu.
func (m *RunManager) pruneDrainingLocked() {
	now := time.Now()
	kept := m.draining[:0]
	for _, run := range m.draining {
		if !run.drainedAt.IsZero() && now.Sub(run.drainedAt) > m.drainTTL {
			slog.Warn("runs: dropping draining run (TTL expired)", "run_id", run.RunID, "agent_id", run.AgentID, "age", now.Sub(run.drainedAt).Round(time.Second))
			continue
		}
		kept = append(kept, run)
	}
	m.draining = kept
	if len(m.draining) > m.drainQueueCap {
		overflow := m.draining[m.drainQueueCap:]
		m.draining = append([]*Run(nil), m.draining[:m.drainQueueCap]...)
		for _, run := range overflow {
			slog.Warn("runs: dropping draining run (queue cap)", "run_id", run.RunID, "agent_id", run.AgentID)
		}
	}
}

// finishIfReadyCtx is finishIfReady with an explicit context: the deferred
// queue worker uses a background context (the client-side session-call
// timeout bounds it), while the inline fallback passes its short deadline
// so a saturated queue cannot stall the caller.
func (m *RunManager) finishIfReadyCtx(ctx context.Context, run *Run) {
	m.mu.Lock()
	// The worker picked up the job: clear the queued marker so a later
	// Maintain pass may retry a run that was not finishable right now.
	if run != nil {
		run.queued = false
	}
	if run == nil || run.inflight > 0 || run.finishing {
		m.mu.Unlock()
		return
	}
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		m.mu.Unlock()
		return
	}
	run.finishing = true
	m.mu.Unlock()

	if err := m.client.FinishRun(ctx, run.RunID, run.Requests); err != nil {
		m.mu.Lock()
		run.finishing = false
		m.mu.Unlock()
		slog.Warn("runs: finish draining run failed", "run_id", run.RunID, "requests", run.Requests, "err", err)
		return
	}

	m.mu.Lock()
	filtered := m.draining[:0]
	for _, d := range m.draining {
		if d != run {
			filtered = append(filtered, d)
		}
	}
	m.draining = filtered
	m.mu.Unlock()
	m.removeRun(run)
	slog.Debug("runs: run finished", "run_id", run.RunID, "requests", run.Requests)
}

// ReleaseAbandoned releases run after the downstream client's context was
// cancelled mid-chat (issue #53, CLI DELETE-on-exit parity): when this was
// the LAST in-flight request on the run, the run is dropped from the active
// set and FINISHed through the bounded queue so upstream does not keep an
// abandoned agent run alive until rotation. Concurrent requests on the same
// run keep it alive (inflight stays > 0). The decrement and the finish
// decision happen under the manager mutex, so a racing Acquire can never
// lease a run that is about to be finished.
func (m *RunManager) ReleaseAbandoned(run *Run) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	if run.inflight > 0 {
		// Other requests are still in flight on this run: keep it alive.
		m.mu.Unlock()
		return
	}
	// Last lease on the run. If it is still the current run, drop it from
	// the active set so no new acquire reuses it, then FINISH it. A run
	// that already rotated away is owned by the draining queue.
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		delete(m.runs, run.AgentID)
		m.mu.Unlock()
		m.enqueueFinish(run)
		return
	}
	m.mu.Unlock()
	// Rotated already: leave the draining FINISH in charge (it will run
	// once the inflight drain completes; nothing else is leasing it now).
}

// drop removes run from the active set (if it is still current) and the
// draining list.
func (m *RunManager) drop(run *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.runs[run.AgentID]; ok && current == run {
		delete(m.runs, run.AgentID)
	}
	filtered := m.draining[:0]
	for _, d := range m.draining {
		if d != run {
			filtered = append(filtered, d)
		}
	}
	m.draining = filtered
}
