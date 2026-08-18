// Package session implements the FreeBuff free-session lifecycle for a
// single token: create, poll, cache, invalidate, and end — with a
// single-flight refresh so concurrent callers share one upstream request.
//
// Semantics ported from proxy-freebuff lib/sessions.js and
// freebuff2api-quorinex free_session.go:
//   - active: ready until expiresAt-5s
//   - disabled: no instance id needed; proceed without one
//   - queued: waiting room; callers get WaitingRoomError until pollAt
//   - ended/superseded/none: transparently re-created
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/upstream"
)

const (
	// expiryMargin is subtracted from expiresAt before a session is
	// considered ready (mirrors the references' 5s safety margin).
	expiryMargin = 5 * time.Second
	// graceWindow is the 30-minute drain window (FREEBUFF_SESSION_GRACE_MS)
	// after expiresAt where in-flight agent chat completions still succeed.
	graceWindow = 30 * time.Minute
	// maxRefreshIterations bounds the create/poll status loop.
	maxRefreshIterations = 5
	// maxOuterIterations bounds EnsureSession's refresh attempts per call so
	// a pathological upstream (always-expired or never-advancing queue)
	// cannot spin forever.
	maxOuterIterations = 10
	// DefaultFallbackModel is the guaranteed-available model used when a
	// requested model is temporarily unavailable upstream, and the default
	// probe target for token tests / smoke: every account (including
	// limited tier) can use it, unlike an alphabetical-first catalog pick.
	DefaultFallbackModel = "deepseek/deepseek-v4-flash"
	// asyncReAdmitTimeout bounds the background pre-emptive re-admit
	// (issue #99) so a hung upstream never leaks a goroutine.
	asyncReAdmitTimeout = time.Minute

	// Terminal-event reasons (T9): the standardized session/invalidation
	// cause vocabulary shared by every terminal session log line. The
	// poll/refresh drop paths map upstream statuses through tableReason;
	// InvalidateWithReason accepts these so callers can name the cause.
	reasonEnded      = "ended"
	reasonSuperseded = "superseded"
	reasonShutdown   = "shutdown"
	reasonModelLock  = "model_lock"
	reasonExpired    = "expired"
	reason409        = "409"
	reasonPoll       = "poll"
	reasonStore      = "store"

	// Re-admit storm detector (T10): more than stormThreshold terminal
	// session events within stormWindow is a session re-admit storm — each
	// invalidation is followed by a fresh admission that burns a daily
	// session slot, so the burst is surfaced once (one Info summary) and
	// re-armed only after a full quiet window passes.
	stormWindow    = 60 * time.Second
	stormThreshold = 3
)

// WaitingRoomError is returned when the session is queued and pollAt has not
// passed. Callers should surface it as 503 with Retry-After.
type WaitingRoomError struct {
	Position   int
	QueueDepth int
	RetryAfter time.Duration
}

func (e *WaitingRoomError) Error() string {
	return fmt.Sprintf("waiting room: position %d of %d, retry after %s", e.Position, e.QueueDepth, e.RetryAfter)
}

// Manager owns the cached session state for one token.
type Manager struct {
	client *upstream.Client

	// store, when non-nil, persists the cached session state across process
	// restarts (SESSION_PERSIST). key is the stable store key derived from
	// the client token (upstream.Client.TokenKey).
	store *Store
	key   string

	mu         sync.Mutex
	state      *cachedState
	refreshCh  chan struct{} // closed by the in-flight refresher when done
	refreshing bool
	// refreshErr retains the last refresh's error under mu so waiters parked
	// on that refresh surface it (after one state re-check) instead of each
	// becoming the next refresher and re-running the failing upstream create.
	// Cleared when a new refresh starts, so a later caller retries normally.
	refreshErr error

	// reAdmitLead (issue #99, SESSION_RE_ADMIT_LEAD default 60s): when the
	// cached active session has less than this much time left, EnsureSession
	// triggers a pre-emptive async re-admit (single-flight through the
	// existing refreshing machinery) and rides the old session; the next
	// request gets the new instance. 0 disables.
	reAdmitLead time.Duration
	// reAdmitExpiry (issue #132) is the expiresAt of the session the last
	// pre-emptive re-admit was triggered for. A failed re-admit must not be
	// re-triggered on every subsequent request in the lead window (each
	// trigger is an upstream session create, and the upstream refuses fresh
	// instances while the old is still authoritative — a 30-create storm
	// was observed). The guard resets naturally when a new session (with a
	// new expiresAt) lands. Guarded by mu.
	reAdmitExpiry time.Time
	// probeTTL (issue #60, SESSION_PROBE_CACHE_TTL default 15s) + lastAdmitted:
	// the last successful upstream session response is reused to skip a
	// redundant poll GET within the TTL.
	probeTTL     time.Duration
	lastAdmitted time.Time

	// adopt is the issue #97 CLI-session adoption mode (ADOPT_CLI_SESSION):
	// nil (default) = create sessions normally. When set, the manager adopts
	// the CLI's active instance and refuses to create a competing session
	// while the CLI process is alive.
	adopt *CLIAdoption

	// now returns the current time; injectable in tests to drive the
	// re-admit storm detector deterministically. Defaults to time.Now.
	now func() time.Time

	// invalidationEvents is the rolling stormWindow of terminal session
	// events (timestamps + reason) feeding the re-admit storm detector
	// (T10); reAdmitTriggers records pre-emptive re-admit trigger times so
	// a storm summary can report how many daily slots the burst burned;
	// lastStormAt suppresses repeat summaries until a quiet window passes.
	invalidationEvents []invalidationEvent
	reAdmitTriggers    []time.Time
	lastStormAt        time.Time
}

// invalidationEvent is one terminal session event in the re-admit storm
// window (T10): when the cached session was dropped and why.
type invalidationEvent struct {
	at     time.Time
	reason string
}

type cachedState struct {
	status             string
	instanceID         string
	model              string
	expiresAt          time.Time
	gracePeriodEndsAt  time.Time
	position           int
	queueDepth         int
	pollAt             time.Time
	accessTier         string
	countryCode        string
	countryBlockReason string
	// ipPrivacySignals / activeUsersForIP / limit are surfaced for the
	// passive ban-risk view (#64): the upstream's own egress classification
	// and the ip_capped admission pressure. Kept out of the persisted store
	// (ephemeral diagnostics; zero after a restart until the next refresh).
	ipPrivacySignals []string
	activeUsersForIP int
	limit            float64
	// quotaByModel is the live per-model session quota from the last
	// admission/poll that carried rateLimitsByModel (key = model id);
	// nil until such a response is seen.
	quotaByModel map[string]upstream.ModelQuota
	// standing is the upstream account standing block (issue #96); nil until
	// an admission/poll that carried it.
	standing *upstream.SessionStanding
}

// NewManager builds a session manager for the given upstream client.
func NewManager(client *upstream.Client) *Manager {
	return NewManagerWithStore(client, nil)
}

// NewManagerWithStore builds a session manager that also persists its cached
// state through store (nil disables persistence).
func NewManagerWithStore(client *upstream.Client, store *Store) *Manager {
	m := &Manager{client: client, store: store, now: time.Now}
	if client != nil {
		m.key = client.TokenKey()
	}
	return m
}

// SetReAdmitLead configures the pre-emptive re-admit lead (issue #99): when
// the cached active session has less than d left, EnsureSessionForModel
// triggers an async re-admit and rides the old session. d <= 0 disables.
// Wired by the pool from SESSION_RE_ADMIT_LEAD; safe to call at runtime.
func (m *Manager) SetReAdmitLead(d time.Duration) {
	m.mu.Lock()
	m.reAdmitLead = d
	m.mu.Unlock()
}

// SetAdmissionProbeTTL configures the admission probe cache TTL (issue #60):
// session poll GETs within d of the last successful session response are
// skipped. d <= 0 disables. Wired by the pool from SESSION_PROBE_CACHE_TTL;
// safe to call at runtime.
func (m *Manager) SetAdmissionProbeTTL(d time.Duration) {
	m.mu.Lock()
	m.probeTTL = d
	m.mu.Unlock()
}

// CLIOwner mirrors the official CLI's freebuff-instance-owner.json (issue
// #97, reference proxy-freebuff server.js readCliInstanceOwner): the CLI
// rewrites this file whenever its active session changes (restart,
// rotation, new conversation).
type CLIOwner struct {
	InstanceID string `json:"instanceId"`
	PID        int    `json:"pid"`
}

// CLIAdoption is the issue #97 opt-in wiring: with ADOPT_CLI_SESSION the
// proxy behaves like the official CLI for a single account — it adopts the
// CLI's ACTIVE session instance and never creates a competing one while the
// CLI process is alive. Enabled is false by default; OwnerFile is the
// freebuff-instance-owner.json path; Initial is the startup snapshot (the
// file is re-read before every refresh). testAlive overrides the PID
// liveness check in tests (nil = platform check).
type CLIAdoption struct {
	Enabled   bool
	OwnerFile string
	Initial   CLIOwner
	testAlive func(int) bool
}

// SetCLIAdoption configures (or clears, with a zero value) the CLI-session
// adoption mode (issue #97, ADOPT_CLI_SESSION). Wired by main.go before the
// pool starts serving.
func (m *Manager) SetCLIAdoption(a CLIAdoption) {
	m.mu.Lock()
	if a.Enabled {
		a.testAlive = processAlive
		m.adopt = &a
	} else {
		m.adopt = nil
	}
	m.mu.Unlock()
}

// adoptOwner re-reads the CLI owner file fresh (issue #97(c)): the CLI
// rewrites freebuff-instance-owner.json when its session changes, so a
// startup snapshot alone would go stale after a CLI restart.
func (m *Manager) adoptOwner() (CLIOwner, bool) {
	m.mu.Lock()
	adopt := m.adopt
	m.mu.Unlock()
	if adopt == nil || !adopt.Enabled {
		return CLIOwner{}, false
	}
	data, err := os.ReadFile(adopt.OwnerFile)
	if err != nil {
		return CLIOwner{}, false
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var owner CLIOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return CLIOwner{}, false
	}
	if owner.InstanceID == "" && owner.PID == 0 {
		// Empty owner record: fall back to the startup snapshot only for the
		// instance id (the pid is required for liveness).
		owner = adopt.Initial
	}
	return owner, true
}

// adoptOrCreate is the issue #97 session-creation path: when CLI adoption
// is enabled it adopts the CLI's active session (or refuses to create a
// competing one); otherwise it creates a fresh session exactly as before.
func (m *Manager) adoptOrCreate(ctx context.Context, requestedModel string) (*upstream.SessionState, error) {
	m.mu.Lock()
	adopt := m.adopt
	m.mu.Unlock()
	if adopt == nil || !adopt.Enabled {
		return m.client.CreateSessionForModel(ctx, requestedModel)
	}

	owner, ok := m.adoptOwner()
	if !ok {
		// Owner file missing/unreadable (issue #97(c)): the CLI's session
		// state is unknown, so a create could supersede and log out the CLI.
		// Refuse loudly rather than compete.
		slog.Warn("ADOPT_CLI_SESSION: freebuff-instance-owner.json missing — refusing to create a competing session",
			"file", adopt.OwnerFile)
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: freebuff-instance-owner.json missing (%s) — refusing to create a competing session (start the CLI once or disable ADOPT_CLI_SESSION)", adopt.OwnerFile)
	}
	if owner.PID <= 0 || !adopt.testAlive(owner.PID) {
		// The CLI process is not running: the proxy may create (and own) a
		// session for the account, exactly like the reference fallback.
		return m.client.CreateSessionForModel(ctx, requestedModel)
	}
	if owner.InstanceID == "" {
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: the FreeBuff CLI is running but no session instance was recorded — refusing to create a competing session (stop the CLI or retry)")
	}
	// CLI alive: adopt ITS session — poll it, never POST a competing one
	// (a create supersedes the CLI's session and logs it out).
	st, err := m.client.GetSession(ctx, owner.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: CLI session %s could not be verified (%v) — refusing to create a competing session (stop the CLI or retry)", shortInstance(owner.InstanceID), err)
	}
	status := strings.TrimSpace(st.Status)
	switch status {
	case "active":
		if requestedModel != "" && st.Model != "" && st.Model != requestedModel {
			return nil, fmt.Errorf("ADOPT_CLI_SESSION: the CLI session is for model %s but %s was requested — refusing to create a competing session (use %s or stop the CLI)", st.Model, requestedModel, st.Model)
		}
		slog.Info("adopted existing CLI freebuff session", "instance_id", shortInstance(st.InstanceID), "model", st.Model)
		return st, nil
	case "queued":
		// Adopt the queue position: pollAt mirrors the create path.
		if st.PollAt.IsZero() {
			wait := time.Duration(st.EstimatedWaitMs) * time.Millisecond
			if wait < time.Second {
				wait = time.Second
			}
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
			st.PollAt = time.Now().Add(wait)
		}
		slog.Info("adopted queued CLI freebuff session", "instance_id", shortInstance(st.InstanceID), "position", st.Position)
		return st, nil
	case "disabled":
		slog.Info("adopted disabled CLI freebuff session")
		return st, nil
	default:
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: CLI session %s is not adoptable (status %q) — refusing to create a competing session (restart the CLI or stop it)", shortInstance(owner.InstanceID), status)
	}
}

// shortInstance renders a session instance id's first 8 chars for logs.
func shortInstance(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// sessionUsable reports whether the cached state can serve a chat right now:
// an active session until expiresAt-5s (the reference safety margin), or any
// state that still holds a live instance id within the 30-minute grace drain
// after expiry (gap #13, FREEBUFF_SESSION_GRACE_MS: within grace the row
// stays alive and chat passes — reference/freebuff freebuff-session.ts). The
// instance-id test guards the grace extension: an ended row whose instance id
// is gone cannot be ridden, and an expired active cache is only reusable
// while its slot survives upstream.
func sessionUsable(s *cachedState) bool {
	if s == nil || s.instanceID == "" {
		return false
	}
	if s.status == "active" && time.Now().Before(s.expiresAt.Add(-expiryMargin)) {
		return true
	}
	graceEnd := s.gracePeriodEndsAt
	if graceEnd.IsZero() && !s.expiresAt.IsZero() {
		graceEnd = s.expiresAt.Add(graceWindow)
	}
	return !graceEnd.IsZero() && time.Now().Before(graceEnd)
}

// commit replaces the cached state and mirrors it into the store (when
// configured). Caller must hold m.mu.
//
// A nil cs removes the store entry conditionally on the instance id being
// dropped: the entry is only deleted while it still belongs to the session
// being invalidated, so a stale commit cannot clobber a persisted slot that
// was replaced concurrently (e.g. a restart re-adopting a different one).
func (m *Manager) commit(cs *cachedState) {
	oldInstance := ""
	if m.state != nil {
		oldInstance = m.state.instanceID
	}
	m.state = cs
	if m.store != nil && m.key != "" {
		if cs == nil {
			m.store.Remove(m.key, oldInstance)
		} else {
			m.store.Save(m.key, cs)
		}
	}
}

// EnsureSession returns the session instance id for the default model, or ""
// when the upstream session is disabled.
func (m *Manager) EnsureSession(ctx context.Context) (string, error) {
	return m.EnsureSessionForModel(ctx, "")
}

// EnsureSessionForModel returns the session instance id bound to the requested
// model. If the session is currently active on a different model, it automatically
// switches models by releasing the previous slot.
func (m *Manager) EnsureSessionForModel(ctx context.Context, model string) (string, error) {
	for attempts := 0; attempts < maxOuterIterations; attempts++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		m.mu.Lock()
		s := m.state
		if s != nil && !m.refreshing {
			switch s.status {
			case "active", "ended":
				// Fast path: reuse the cached instance while it is usable —
				// an active session until expiresAt-5s, or (gap #13) a
				// session whose instance id survives the 30-minute grace
				// drain (FREEBUFF_SESSION_GRACE_MS: within grace the row
				// stays alive and chat passes).
				if (model == "" || s.model == "" || s.model == model) && sessionUsable(s) {
					instance := s.instanceID
					// Issue #99: pre-emptive re-admit. Only while the
					// session is still pre-expiry (within reAdmitLead of
					// expiresAt-5s): inside the grace drain the CLI rides
					// the old row until grace ends, and a fresh admission
					// would supersede it. The refresh runs on a background
					// context so the caller's cancellation never strands a
					// half-started admission.
					if s.status == "active" && m.reAdmitLead > 0 &&
						time.Now().Before(s.expiresAt.Add(-expiryMargin)) &&
						time.Until(s.expiresAt.Add(-expiryMargin)) <= m.reAdmitLead &&
						!m.reAdmitExpiry.Equal(s.expiresAt) {
						// Issue #132: one attempt per expiry window. The
						// upstream refuses a fresh create while the old
						// instance is still authoritative, so a failed
						// re-admit must ride the old session to expiry
						// instead of re-triggering on every request (each
						// trigger burns a session slot).
						m.reAdmitExpiry = s.expiresAt
						m.refreshing = true
						m.refreshErr = nil
						refreshCh := make(chan struct{})
						m.refreshCh = refreshCh
						m.mu.Unlock()
						go m.asyncReAdmit(model)
						m.recordReAdmitTrigger()
						slog.Debug("session: pre-emptive re-admit triggered", "instance_id", instance, "model", s.model)
						return instance, nil
					}
					m.mu.Unlock()
					slog.Debug("session reused", "instance_id", instance, "model", s.model, "expires_at", s.expiresAt.Format(time.RFC3339))
					return instance, nil
				}
				// Usability exhausted (past grace) or model mismatch — fall
				// through to refresh.
			case "disabled":
				m.mu.Unlock()
				return "", nil
			case "queued":
				if now := time.Now(); now.Before(s.pollAt) {
					wa := WaitingRoomError{
						Position:   s.position,
						QueueDepth: s.queueDepth,
						RetryAfter: s.pollAt.Sub(now),
					}
					m.mu.Unlock()
					return "", &wa
				}
				// pollAt passed — fall through to refresh and advance.
			}
		}
		if m.refreshing {
			// Another caller is the refresher: park on its completion signal.
			refreshCh := m.refreshCh
			m.mu.Unlock()
			select {
			case <-refreshCh:
				// The refresh finished. If it failed, surface its retained
				// error to every waiter (after one state re-check) instead of
				// letting each waiter become the next refresher and re-run
				// the failing upstream create (N callers → N serial POSTs).
				m.mu.Lock()
				err := m.refreshErr
				m.mu.Unlock()
				if err != nil {
					if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil {
						// Leader goroutine was canceled or timed out, but this waiter's
						// context is still active. Loop back to become a candidate leader
						// and refresh rather than propagating the aborted leader's error.
						continue
					}
					m.mu.Lock()
					s = m.state
					m.mu.Unlock()
					// One state re-check: the failed refresh may still have
					// advanced the queue (e.g. to queued with a future
					// pollAt) — honor that before surfacing the error.
					if s != nil && s.status == "queued" && time.Now().Before(s.pollAt) {
						return "", &WaitingRoomError{
							Position:   s.position,
							QueueDepth: s.queueDepth,
							RetryAfter: time.Until(s.pollAt),
						}
					}
					// Issue #99: a failed pre-emptive re-admit leaves the old
					// session authoritative — ride it rather than erroring a
					// request that could still be served (through the grace
					// drain, gap #13).
					if s != nil && sessionUsable(s) {
						return s.instanceID, nil
					}
					return "", err
				}
				continue // refresh succeeded; loop re-evaluates cached state
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		// We are the refresher. Run the create/poll loop outside the lock and
		// clear any previously retained refresh error.
		m.refreshing = true
		m.refreshErr = nil
		refreshCh := make(chan struct{})
		m.refreshCh = refreshCh
		m.mu.Unlock()

		err := m.refresh(ctx, model, false)
		m.mu.Lock()
		m.refreshing = false
		if err != nil {
			m.refreshErr = err
		}
		close(m.refreshCh)
		m.refreshCh = nil
		m.mu.Unlock()
		if err != nil {
			return "", err
		}

		// Freshly refreshed: trust the new state.
		m.mu.Lock()
		s = m.state
		m.mu.Unlock()
		if s == nil {
			continue // ended/superseded cleared it; refresh again
		}
		switch s.status {
		case "active":
			return s.instanceID, nil
		case "disabled":
			return "", nil
		case "queued":
			if now := time.Now(); now.Before(s.pollAt) {
				return "", &WaitingRoomError{
					Position:   s.position,
					QueueDepth: s.queueDepth,
					RetryAfter: s.pollAt.Sub(now),
				}
			}
		}
	}
	return "", errors.New("session: not ready after repeated refreshes")
}

// asyncReAdmit runs a pre-emptive refresh in the background (issue #99): the
// triggering request rides the old session while the new admission proceeds;
// concurrent requests park on the single-flight refreshCh and get the new
// instance once it lands (or ride the old session when the refresh fails and
// it is still usable). Bounded by asyncReAdmitTimeout so a hung upstream
// never leaks a goroutine.
func (m *Manager) asyncReAdmit(model string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncReAdmitTimeout)
	defer cancel()
	err := m.refresh(ctx, model, true)
	m.mu.Lock()
	m.refreshing = false
	if err != nil {
		m.refreshErr = err
	}
	close(m.refreshCh)
	m.refreshCh = nil
	m.mu.Unlock()
	if err != nil {
		slog.Debug("session: pre-emptive re-admit failed", "err", err)
		return
	}
	slog.Debug("session: pre-emptive re-admit done")
}

// recordReAdmitTrigger remembers a pre-emptive re-admit trigger (issue #99)
// for the re-admit storm summary's burned_slots count (T10): a trigger whose
// session is later invalidated burned a daily session slot. Caller must NOT
// hold m.mu.
func (m *Manager) recordReAdmitTrigger() {
	m.mu.Lock()
	now := m.now()
	cutoff := now.Add(-stormWindow)
	m.reAdmitTriggers = append(m.reAdmitTriggers, now)
	triggers := m.reAdmitTriggers[:0]
	for _, t := range m.reAdmitTriggers {
		if t.After(cutoff) {
			triggers = append(triggers, t)
		}
	}
	m.reAdmitTriggers = triggers
	m.mu.Unlock()
}

// recordInvalidation appends a terminal session event to the rolling
// re-admit storm window (T10) and, when more than stormThreshold
// invalidations land within stormWindow and the suppression window has
// passed, emits ONE Info summary (count, duration_ms, superseded,
// burned_slots). Caller must NOT hold m.mu; the summary is logged outside
// the lock.
func (m *Manager) recordInvalidation(reason string) {
	m.mu.Lock()
	now := m.now()
	m.invalidationEvents = append(m.invalidationEvents, invalidationEvent{at: now, reason: reason})
	cutoff := now.Add(-stormWindow)
	events := m.invalidationEvents[:0]
	for _, ev := range m.invalidationEvents {
		if ev.at.After(cutoff) {
			events = append(events, ev)
		}
	}
	m.invalidationEvents = events
	triggers := m.reAdmitTriggers[:0]
	for _, t := range m.reAdmitTriggers {
		if t.After(cutoff) {
			triggers = append(triggers, t)
		}
	}
	m.reAdmitTriggers = triggers

	// Storm only when strictly more than the threshold invalidations sit in
	// the window, and only once per suppression window (60s of quiet re-arms
	// the detector).
	if len(m.invalidationEvents) <= stormThreshold || (!m.lastStormAt.IsZero() && now.Sub(m.lastStormAt) < stormWindow) {
		m.mu.Unlock()
		return
	}
	m.lastStormAt = now
	count := len(m.invalidationEvents)
	duration := m.invalidationEvents[len(m.invalidationEvents)-1].at.Sub(m.invalidationEvents[0].at).Milliseconds()
	superseded := 0
	for _, ev := range m.invalidationEvents {
		if ev.reason == reasonSuperseded {
			superseded++
		}
	}
	// burned_slots: pre-emptive re-admit triggers within the same window —
	// each one whose session the storm then invalidated burned a daily slot.
	// The trigger list is pruned to the window above, so its length is the
	// count.
	burned := len(m.reAdmitTriggers)
	m.mu.Unlock()

	slog.Info("session re-admit storm",
		"count", count,
		"duration_ms", duration,
		"superseded", superseded,
		"burned_slots", burned)
}

// tableReason maps an upstream session status to the terminal-event reason
// vocabulary (T9). Used by the poll/refresh drop paths so the logged reason
// is always one of the table values; the raw upstream status rides in the
// log's status field.
func tableReason(status string) string {
	if status == "superseded" {
		return reasonSuperseded
	}
	return reasonEnded
}

// statusError maps an upstream session status to the typed error callers
// use for recovery (token cooldown, region surfacing). st supplies the
// fields carried by the error; non-error statuses return nil. Shared by
// refresh and Poll so both map the same way.
func statusError(status string, st *upstream.SessionState) error {
	switch status {
	case "banned":
		return &upstream.BanError{ResumesAt: st.ResumesAt, Body: st.Message}
	case "country_blocked":
		return &upstream.CountryBlockedError{
			CountryCode:        st.CountryCode,
			CountryBlockReason: st.CountryBlockReason,
			IpPrivacySignals:   st.IpPrivacySignals,
		}
	case "rate_limited", "spend_limited":
		retryAfter := time.Duration(st.RetryAfterMs) * time.Millisecond
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.RateLimitError{
			Status:      status,
			RetryAfter:  retryAfter,
			ResetAt:     st.ResetAt,
			Limit:       st.Limit,
			RecentCount: st.RecentCount,
			Body:        st.Message,
		}
	case "ip_capped":
		// Distinct error: ip_capped is admission-only (too many distinct
		// users on the egress IP) and NOT tied to a quota reset, so the
		// cooldown is bounded to retryAfterMs only — never the
		// Pacific-midnight lock (reference/freebuff freebuff-session.ts).
		retryAfter := time.Duration(st.RetryAfterMs) * time.Millisecond
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.IpCappedError{
			ActiveUsersForIP: st.ActiveUsersForIP,
			Limit:            st.Limit,
			RetryAfter:       retryAfter,
			Body:             st.Message,
		}
	case "session_model_mismatch", "limited_ip":
		// The egress IP cannot serve the requested model. The session row is
		// fine (bound to its admitted model) — not session-invalid, so it
		// must never be invalidated/refreshed (re-admitting burns a daily
		// session slot). Non-limited messages keep today's exact error text.
		if strings.Contains(strings.ToLower(st.Message), "limited") {
			return &upstream.LimitedIpError{
				RetryAfter: time.Duration(st.RetryAfterMs) * time.Millisecond,
				Body:       st.Message,
			}
		}
		return fmt.Errorf("session: unknown upstream status %q", status)
	}
	return nil
}

// refresh runs the create/poll status loop, updating cached state, until the
// session is active, disabled, or the iteration budget is exhausted.
// preemptive marks an issue #99 async re-admit: a create refusal while the
// old instance is still authoritative must NOT invalidate the cached session
// (the caller is riding it) — return instead of committing nil and looping.
func (m *Manager) refresh(ctx context.Context, requestedModel string, preemptive bool) error {
	targetModel := requestedModel
	for i := 0; i < maxRefreshIterations; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		cached := m.state
		m.mu.Unlock()

		var (
			st  *upstream.SessionState
			err error
		)
		if cached != nil && cached.status == "queued" && cached.instanceID != "" {
			st, err = m.client.GetSession(ctx, cached.instanceID)
		} else if cached == nil {
			// Fresh manager (first call or restart): resume a persisted
			// session before creating a new one. A persisted active slot
			// that is still alive upstream (and model-compatible) is adopted
			// instead of burning a fresh session quota.
			st, err = m.pollPersisted(ctx, targetModel)
			if st == nil && err == nil {
				st, err = m.adoptOrCreate(ctx, targetModel)
			}
		} else {
			// Live refresh (expired cache or model mismatch): never consult
			// the store. Re-adopting a persisted slot here would pin the
			// previous model's session on every refresh; always create for
			// the requested model (baseline behavior).
			st, err = m.adoptOrCreate(ctx, targetModel)
		}
		if err != nil {
			return err
		}

		status := st.Status
		switch status {
		case "active":
			model := st.Model
			if model == "" {
				model = targetModel
			}
			m.mu.Lock()
			m.commit(&cachedState{
				status:             "active",
				instanceID:         st.InstanceID,
				model:              model,
				expiresAt:          st.ExpiresAt,
				gracePeriodEndsAt:  st.ExpiresAt.Add(graceWindow),
				accessTier:         st.AccessTier,
				countryCode:        st.CountryCode,
				countryBlockReason: st.CountryBlockReason,
				activeUsersForIP:   st.ActiveUsersForIP,
				ipPrivacySignals:   st.IpPrivacySignals,
				limit:              st.Limit,
				quotaByModel:       st.RateLimitsByModel,
				standing:           st.Standing,
			})
			// Issue #60: the successful admission refreshes the probe cache
			// window — subsequent session poll GETs within the TTL are
			// skipped.
			m.lastAdmitted = time.Now()
			m.mu.Unlock()
			slog.Debug("session created", "status", "active", "instance_id", st.InstanceID,
				"model", model, "expires_at", st.ExpiresAt.Format(time.RFC3339))
			return nil
		case "disabled":
			m.mu.Lock()
			m.commit(&cachedState{status: "disabled"})
			m.mu.Unlock()
			slog.Debug("session created", "status", "disabled", "instance_id", "")
			return nil
		case "queued":
			pollAt := st.PollAt
			if pollAt.IsZero() {
				wait := time.Duration(st.EstimatedWaitMs) * time.Millisecond
				if wait < time.Second {
					wait = time.Second
				}
				if wait > 5*time.Second {
					wait = 5 * time.Second
				}
				pollAt = time.Now().Add(wait)
			}
			model := st.Model
			if model == "" {
				model = targetModel
			}
			m.mu.Lock()
			m.commit(&cachedState{
				status:     "queued",
				instanceID: st.InstanceID,
				model:      model,
				position:   st.Position,
				queueDepth: st.QueueDepth,
				pollAt:     pollAt,
			})
			m.mu.Unlock()
			slog.Debug("session queued", "instance_id", st.InstanceID, "model", model,
				"position", st.Position, "queue_depth", st.QueueDepth, "poll_at", pollAt.Format(time.RFC3339))
			return nil
		case "ended", "superseded", "none":
			if preemptive {
				// Issue #132: the upstream refused a fresh admission while
				// the old instance is still authoritative (the re-admit
				// overlap). Keep the cached session — the triggering
				// request is riding it until expiry — and stop; the
				// once-per-expiry guard prevents a retry storm.
				return errors.New("session: pre-emptive re-admit refused (old session still active)")
			}
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
			m.recordInvalidation(tableReason(status))
			slog.Debug("session recreated", "reason", tableReason(status), "status", status, "instance_id", st.InstanceID)
		case "banned", "country_blocked", "rate_limited", "ip_capped", "spend_limited", "session_model_mismatch", "limited_ip":
			return statusError(status, st)
		case "model_locked":
			// Previous session is locked to a different model.
			// Release the old slot and retry with the desired model.
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
			m.recordInvalidation(reasonModelLock)
			_ = m.client.EndSession(ctx)
			slog.Debug("session released on model lock, retrying", "reason", reasonModelLock, "current", st.CurrentModel, "target", targetModel)
		case "model_unavailable":
			// Requested model is not available; fall back to default model.
			slog.Warn("session: model unavailable upstream, falling back to default", "requested", targetModel, "fallback", DefaultFallbackModel)
			targetModel = DefaultFallbackModel
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
		default:
			return fmt.Errorf("session: unknown upstream status %q", status)
		}
	}
	return errors.New("session: refresh iteration budget exhausted")
}

// SessionSnapshot is a lock-free best-effort view of the cached session
// state, for healthz-style reporting (pool.TokenSnapshot).
type SessionSnapshot struct {
	Status        string
	InstanceID    string
	Model         string
	QueuePosition int
	QueueDepth    int
	TierAccess    string
	// CountryCode is the admitted session's country ("" when absent).
	CountryCode        string
	TierCountry        string
	CountryBlockReason string
	// ActiveUsersForIP is the last known distinct-user count on the token's
	// egress IP (upstream activeUsersForIp); 0 when absent.
	ActiveUsersForIP int
	// IPPrivacySignals is the upstream's own egress-IP classification
	// (e.g. vpn/proxy/tor/hosting); Limit is the ip_capped ceiling. Both
	// feed the passive ban-risk view (#64); empty/0 when absent.
	IPPrivacySignals []string
	Limit            float64
	ExpiresAt        time.Time
	// GracePeriodEndsAt is when the 30-minute drain window after ExpiresAt
	// closes (previously computed but never surfaced).
	GracePeriodEndsAt time.Time
	// QuotaByModel carries the live per-model session quotas (key = model id).
	// Entitlement is a top-level per-token view; it stays empty because the
	// upstream wire nests entitlement inside each rate-limit entry.
	QuotaByModel map[string]QuotaSnapshot
	Entitlement  map[string]float64
	// Standing is the upstream account standing block (issue #96); nil until
	// an admission/poll that carried it.
	Standing *upstream.SessionStanding
}

// QuotaSnapshot is one model's live session quota for healthz/metrics
// reporting (pool.TokenSnapshot). Mirrors upstream.ModelQuota.
type QuotaSnapshot struct {
	Model       string
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	Period      string
	Entitlement map[string]float64
}

// Snapshot returns a best-effort view of the cached session state. All
// fields may be zero when no session has been created yet. Added for
// internal/pool snapshotting; no upstream calls are made.
func (m *Manager) Snapshot() SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return SessionSnapshot{}
	}
	quota := make(map[string]QuotaSnapshot, len(m.state.quotaByModel))
	for modelID, q := range m.state.quotaByModel {
		quota[modelID] = QuotaSnapshot{
			Model:       q.Model,
			Limit:       q.Limit,
			RecentCount: q.RecentCount,
			ResetAt:     q.ResetAt,
			Period:      q.Period,
			Entitlement: q.Entitlement,
		}
	}
	return SessionSnapshot{
		Status:             m.state.status,
		InstanceID:         m.state.instanceID,
		Model:              m.state.model,
		QueuePosition:      m.state.position,
		QueueDepth:         m.state.queueDepth,
		TierAccess:         m.state.accessTier,
		CountryCode:        m.state.countryCode,
		TierCountry:        m.state.countryCode,
		CountryBlockReason: m.state.countryBlockReason,
		ActiveUsersForIP:   m.state.activeUsersForIP,
		IPPrivacySignals:   m.state.ipPrivacySignals,
		Limit:              m.state.limit,
		ExpiresAt:          m.state.expiresAt,
		GracePeriodEndsAt:  m.state.gracePeriodEndsAt,
		QuotaByModel:       quota,
		Standing:           m.state.standing,
	}
}

// Invalidate drops the cached session so the next EnsureSession re-creates
// it. Used when a chat request reports a session-level error. The
// invalidation is recorded with the canonical 409 reason (the session-invalid
// chat family); callers that can name a more specific cause use
// InvalidateWithReason.
func (m *Manager) Invalidate() {
	m.InvalidateWithReason(reason409, 0)
}

// InvalidateWithReason drops the cached session, recording WHY (T9/T10) and
// feeding the re-admit storm detector. reason is a terminal-event cause from
// the vocabulary (ended|superseded|shutdown|model_lock|expired|409|poll|
// store); status is the triggering HTTP status when known (e.g. 409 from the
// chat/poll error), 0 when unknown — a 0 status is omitted from the log.
func (m *Manager) InvalidateWithReason(reason string, status int) {
	m.mu.Lock()
	instanceID := ""
	if m.state != nil {
		instanceID = m.state.instanceID
	}
	m.commit(nil)
	m.mu.Unlock()
	m.recordInvalidation(reason)
	if status > 0 {
		slog.Debug("session invalidated", "instance_id", instanceID, "reason", reason, "status", status)
		return
	}
	slog.Debug("session invalidated", "instance_id", instanceID, "reason", reason)
}

// InvalidateInstance drops the cached session only when its instance id
// matches instanceID (issue #132): after a pre-emptive re-admit lands a new
// instance, a chat that was still riding the OLD (superseded) instance
// failing must not invalidate the NEW cached session — that would force the
// next request to re-create and restart the churn. A mismatch leaves the
// cache untouched.
func (m *Manager) InvalidateInstance(instanceID string) {
	if instanceID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.instanceID != instanceID {
		return
	}
	m.commit(nil)
	slog.Debug("session invalidated", "instance_id", instanceID)
}

// ClearQueued drops the cached session only when it is in the queued
// (waiting-room) state, and reports whether it did (issue #100: the
// queue-time model fallback clears queued caches so a fallback-model
// acquire can create a fresh session instead of re-surfacing the same
// waiting room). Active/disabled states are untouched.
func (m *Manager) ClearQueued() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.status == "queued" {
		m.commit(nil)
		return true
	}
	return false
}

// EndSession deletes the upstream session (if any) and clears the cache.
func (m *Manager) EndSession(ctx context.Context) error {
	m.mu.Lock()
	instanceID := ""
	if s := m.state; s != nil {
		instanceID = s.instanceID
	}
	m.commit(nil)
	m.mu.Unlock()

	if instanceID == "" {
		return nil
	}
	slog.Debug("session ended", "instance_id", instanceID, "reason", reasonEnded)
	// A superseded DELETE is the same "slot already gone" case as
	// session-invalid (#119): swallow both so teardown never errors on a
	// slot another instance took over.
	if err := m.client.EndSession(ctx); err != nil && !errors.Is(err, upstream.ErrSessionInvalid) && !errors.Is(err, upstream.ErrSessionSuperseded) {
		return err
	}
	return nil
}

// Shutdown handles session teardown at process shutdown. Per the CLI
// (gap #13), exit ALWAYS releases the upstream session slot (DELETE),
// whether or not persistence is enabled — the CLI DELETEs on exit and a
// later restart re-admits fresh. When persistence is enabled the cached
// state is flushed to the store FIRST (so a crash mid-shutdown does not
// lose the entry) and the entry survives the DELETE: a restart resumes via
// pollPersisted, which re-adopts the slot when the DELETE did not take
// effect upstream, or drops the dead entry and re-POSTs fresh when it did.
// Runs are FINISHed separately by the run manager.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m.store == nil {
		// No persistence: exactly the normal EndSession path (DELETE
		// upstream + drop the cache).
		return m.EndSession(ctx)
	}
	m.mu.Lock()
	instanceID := ""
	if m.state != nil && m.state.instanceID != "" {
		instanceID = m.state.instanceID
		m.store.Save(m.key, m.state)
		// Surface a failed flush: without the persisted entry a restart
		// cannot resume the slot, so a write/rename failure must not be
		// silent. Re-read the FILE through a fresh Store — the in-memory
		// map is updated before the flush attempt and cannot verify disk.
		if persisted := NewStore(m.store.path).Load(m.key); persisted == nil || persisted.instanceID != instanceID {
			slog.Warn("session: shutdown persist failed", "instance_id", shortInstance(instanceID))
		}
	}
	m.mu.Unlock()

	if instanceID == "" {
		return nil
	}
	// Release the upstream slot directly (not EndSession): EndSession's CAS
	// commit(nil) would remove the store entry we just flushed, and the
	// DELETE is keyed on the user, not the instance (reference/freebuff
	// session wire: DELETE = Bearer only, #120 — EndSession never sends the
	// instance header). The cached state is kept in-memory so the store
	// entry stays; the process is exiting.
	slog.Debug("session ended on shutdown", "instance_id", shortInstance(instanceID), "reason", reasonShutdown)
	if err := m.client.EndSession(ctx); err != nil && !errors.Is(err, upstream.ErrSessionInvalid) && !errors.Is(err, upstream.ErrSessionSuperseded) {
		return err
	}
	return nil
}

// pollPersisted attempts to resume a persisted session before a fresh create
// (restart reuse). requestedModel is the model the caller is about to create
// for: a persisted slot bound to a different model is dropped instead of
// adopted, so a model-mismatch refresh falls through to a create for the
// requested model.
//
// It returns a non-nil SessionState when the persisted slot is still active
// upstream and model-compatible (adopted), and (nil, nil) otherwise — either
// there is no persisted slot, it is expired, it is model-incompatible, or it
// is dead upstream (in which case it is removed from the store). Transport
// errors are returned so the caller surfaces them like a failed create
// instead of burning a fresh daily session slot on a merely-flaky upstream.
func (m *Manager) pollPersisted(ctx context.Context, requestedModel string) (*upstream.SessionState, error) {
	if m.store == nil || m.key == "" {
		return nil, nil
	}
	cs := m.store.Load(m.key)
	if cs == nil || cs.status != "active" || cs.instanceID == "" {
		return nil, nil
	}
	// Only resume a slot that is still genuinely active (with the 5s safety
	// margin). An expired-but-in-grace slot is draining; resume it is not
	// worth the risk of admitting new work onto a dying session.
	if cs.expiresAt.IsZero() || !time.Now().Before(cs.expiresAt.Add(-expiryMargin)) {
		m.store.Remove(m.key, cs.instanceID)
		return nil, nil
	}
	// Model gate (pre-flight): a persisted slot known to be bound to a
	// different model must never be re-adopted for another model — that
	// would pin the old model's session forever on every refresh.
	if cs.model != "" && requestedModel != "" && cs.model != requestedModel {
		m.store.Remove(m.key, cs.instanceID)
		return nil, nil
	}

	st, err := m.client.GetSession(ctx, cs.instanceID)
	if err != nil {
		// Transport error: surface it instead of swallowing and falling
		// through to a fresh create. The caller retries (single-flight /
		// TRANSIENT_RETRIES); a create here would burn a session slot.
		return nil, err
	}
	switch st.Status {
	case "active":
		// Model gate (post-flight): the upstream may have bound the resumed
		// slot to a different model than requested. Adopt only when the
		// resumed model is compatible; otherwise drop the slot and fall
		// through to a create for the requested model.
		if st.Model != "" && requestedModel != "" && st.Model != requestedModel {
			m.store.Remove(m.key, st.InstanceID)
			return nil, nil
		}
		slog.Debug("session resumed from store", "instance_id", st.InstanceID, "expires_at", st.ExpiresAt.Format(time.RFC3339))
		return st, nil
	case "ended", "superseded", "none", "banned":
		m.store.Remove(m.key, st.InstanceID)
		return nil, nil
	case "country_blocked", "rate_limited", "ip_capped", "spend_limited":
		// Terminal admission refusals: the persisted slot is dead upstream;
		// drop it so a restart re-admits from scratch instead of re-polling.
		m.store.Remove(m.key, st.InstanceID)
		return nil, nil
	default:
		// queued or an unknown status: not resumable as an active slot, but
		// non-terminal — keep the entry for a later restart.
		return nil, nil
	}
}

// Poll runs the periodic session-liveness poll: a compact GET with NO
// heartbeat header — the CLI never beats (x-freebuff-heartbeat is
// Desktop-only, reference/freebuff freebuff-models.ts:1212-1215); liveness
// comes from the recurring compact GET itself (gap #2). It refreshes the
// cached state the way the CLI's 30s compact poll does: statusError
// mappings, drop-on-ban, invalidate on superseded/none, and — within the
// 30-minute grace drain — an ended response that still carries the instance
// id is kept as a usable "ended" row instead of being invalidated (gap #13).
func (m *Manager) Poll(ctx context.Context) error {
	m.mu.Lock()
	if m.state == nil || (m.state.status != "active" && m.state.status != "ended") || m.state.instanceID == "" {
		m.mu.Unlock()
		return nil
	}
	// Issue #60: admission probe caching — within the probe TTL of the last
	// successful session response the cached state is authoritative, so the
	// poll GET is redundant; skip it (less upstream traffic, fewer chances
	// to trip the one-client-at-a-time gate).
	if m.probeTTL > 0 && !m.lastAdmitted.IsZero() && time.Since(m.lastAdmitted) < m.probeTTL {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.state.instanceID
	m.mu.Unlock()

	start := time.Now()
	st, err := m.client.GetSessionWithOpts(ctx, instanceID, true)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		// #116: 428 waiting_room_required is session-ENDING
		// (endsTheSession:true per FREEBUFF_GATE_CODES — the seat is gone;
		// reference/freebuff freebuff-session.ts). Drop the cached admission
		// so the next EnsureSession re-admits fresh (the pool's
		// WAITING_ROOM_CHAIN fires before the create). Any other poll error
		// is left for the pool's failure backoff.
		if errors.Is(err, upstream.ErrWaitingRoomRequired) {
			dropped := false
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				dropped = true
			}
			m.mu.Unlock()
			if dropped {
				m.recordInvalidation(reasonPoll)
				slog.Warn("session dropped during poll", "reason", reasonPoll, "status", "waiting_room_required", "instance_id", instanceID)
			}
		}
		return err
	}
	m.mu.Lock()
	// A successful GET confirms the cached state: refresh the probe window.
	m.lastAdmitted = time.Now()
	m.mu.Unlock()
	if serr := statusError(st.Status, st); serr != nil {
		// A banned session is dead until the account unban: drop the cached
		// admission (cooldown) so the token re-admits only after the pool's
		// ban window, instead of polling a stale slot.
		if st.Status == "banned" {
			dropped := false
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				dropped = true
			}
			m.mu.Unlock()
			if dropped {
				m.recordInvalidation(reasonPoll)
				slog.Warn("session dropped during poll", "reason", reasonPoll, "status", st.Status, "instance_id", instanceID)
			}
		}
		return serr
	}
	if st.Status == "superseded" || st.Status == "none" {
		dropped := false
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			dropped = true
		}
		m.mu.Unlock()
		if dropped {
			m.recordInvalidation(tableReason(st.Status))
			slog.Warn("session ended during poll", "reason", tableReason(st.Status), "status", st.Status, "instance_id", instanceID)
		}
		return nil
	}
	if st.Status == "ended" {
		// Ended WITH the instance id still present: the row is in the 30-min
		// grace drain and stays usable (gap #13). Refresh the cached state
		// as ended-with-instance so the fast path keeps serving it until
		// grace closes; the pool keeps polling. The grace end comes from the
		// response when present, else expiresAt + graceWindow.
		graceEnd := st.GracePeriodEndsAt
		if graceEnd.IsZero() && !st.ExpiresAt.IsZero() {
			graceEnd = st.ExpiresAt.Add(graceWindow)
		}
		if st.InstanceID != "" && !graceEnd.IsZero() && time.Now().Before(graceEnd) {
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(&cachedState{
					status:            "ended",
					instanceID:        st.InstanceID,
					model:             m.state.model,
					expiresAt:         st.ExpiresAt,
					gracePeriodEndsAt: graceEnd,
				})
				slog.Debug("session in grace drain during poll", "instance_id", instanceID, "grace_ends_at", graceEnd.Format(time.RFC3339))
			}
			m.mu.Unlock()
			return nil
		}
		// The row is gone (no instance id) or past grace: drop it so the
		// next EnsureSession re-creates a fresh session.
		dropped := false
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			dropped = true
		}
		m.mu.Unlock()
		if dropped {
			m.recordInvalidation(tableReason(st.Status))
			slog.Warn("session ended during poll", "reason", tableReason(st.Status), "status", st.Status, "instance_id", instanceID)
		}
		return nil
	}
	// Heartbeat liveness confirmed: the compact poll returned a usable
	// status (active). instance/ms/status standardize the heartbeat poll
	// line (T11) so ops can see each liveness beat and its latency.
	slog.Debug("session: heartbeat poll", "instance_id", shortInstance(instanceID), "ms", ms, "status", st.Status)
	return nil
}
