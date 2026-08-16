package session

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// storeVersion guards the on-disk format; bump it when the schema changes so
// a stale file is ignored instead of mis-parsed.
const storeVersion = 1

// maxStoreFileSize caps the on-disk store file; anything larger is treated
// as empty instead of being read into memory wholesale.
const maxStoreFileSize = 8 << 20 // 8 MiB

// persistedState is the on-disk shape of one token's cached session. The
// instance id + expiry are the fields that matter for restart-resume; the
// rest are carried so a resumed session keeps its tier/country/queue view.
type persistedState struct {
	InstanceID         string    `json:"instance_id"`
	Model              string    `json:"model"`
	Status             string    `json:"status"`
	ExpiresAt          time.Time `json:"expires_at"`
	GracePeriodEndsAt  time.Time `json:"grace_period_ends_at"`
	Position           int       `json:"position"`
	QueueDepth         int       `json:"queue_depth"`
	PollAt             time.Time `json:"poll_at"`
	AccessTier         string    `json:"access_tier"`
	CountryCode        string    `json:"country_code"`
	CountryBlockReason string    `json:"country_block_reason"`
}

type storeFile struct {
	Version  int                       `json:"version"`
	Sessions map[string]persistedState `json:"sessions"`
}

// Store persists cached session state to a single JSON file so a proxy
// restart can resume an unexpired upstream session instead of burning a new
// session slot. Keys are token hashes (upstream.Client.TokenKey), never raw
// tokens. All methods are safe for concurrent use; writes are atomic
// (temp file + rename) and the file is created with mode 0600.
type Store struct {
	path string

	mu     sync.Mutex
	data   map[string]persistedState
	loaded bool
}

// NewStore builds a store backed by path. The file is read lazily on the
// first Load; NewStore never fails (a missing/unreadable file is treated as
// empty and a later Save overwrites it).
func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.data = make(map[string]persistedState)

	// Reject oversized files before reading them into memory.
	if fi, err := os.Stat(s.path); err == nil && fi.Size() > maxStoreFileSize {
		slog.Warn("session store: file too large, ignoring", "path", s.path, "bytes", fi.Size())
		s.loaded = true
		return
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First run: a missing file is a valid empty store.
			s.loaded = true
		} else {
			// Leave loaded=false so the next Load (or Save) retries the
			// read instead of permanently freezing an empty view that a
			// later Save would flush over the on-disk file, destroying
			// other tokens' persisted sessions.
			slog.Warn("session store: read failed, will retry on next access", "path", s.path, "err", err)
		}
		return
	}
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		// The file is genuinely bad: remember that so we stop re-parsing it
		// and proceed empty (a later Save replaces it).
		slog.Warn("session store: parse failed, ignoring", "path", s.path, "err", err)
		s.loaded = true
		return
	}
	if file.Version != storeVersion {
		slog.Warn("session store: version mismatch, ignoring", "path", s.path, "version", file.Version)
		s.loaded = true
		return
	}
	if file.Sessions != nil {
		for key, ps := range file.Sessions {
			// An "active" entry without an instance id cannot be resumed and
			// would poison the resume path; drop it on load.
			if ps.Status == "active" && ps.InstanceID == "" {
				slog.Warn("session store: dropping active entry with empty instance id", "path", s.path, "key", key)
				continue
			}
			s.data[key] = ps
		}
	}
	s.loaded = true
}

// Load returns the persisted cached state for key, or nil when absent or
// already expired beyond the grace window. Load never performs upstream
// calls; it only filters obviously-dead entries.
func (s *Store) Load(key string) *cachedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	ps, ok := s.data[key]
	if !ok {
		return nil
	}
	// Drop entries whose grace window is already closed: resuming them is
	// impossible and keeping them only delays the inevitable re-create.
	if !ps.GracePeriodEndsAt.IsZero() && time.Now().After(ps.GracePeriodEndsAt) {
		delete(s.data, key)
		return nil
	}
	return &cachedState{
		status:             ps.Status,
		instanceID:         ps.InstanceID,
		model:              ps.Model,
		expiresAt:          ps.ExpiresAt,
		gracePeriodEndsAt:  ps.GracePeriodEndsAt,
		position:           ps.Position,
		queueDepth:         ps.QueueDepth,
		pollAt:             ps.PollAt,
		accessTier:         ps.AccessTier,
		countryCode:        ps.CountryCode,
		countryBlockReason: ps.CountryBlockReason,
	}
}

// Save persists cs under key. A nil cs removes the key. Disabled sessions
// (no instance id, no expiry) are not persisted: there is nothing to resume.
func (s *Store) Save(key string, cs *cachedState) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	if cs == nil || (cs.instanceID == "" && cs.status != "queued") {
		delete(s.data, key)
		s.flushLocked()
		return
	}
	s.data[key] = persistedState{
		InstanceID:         cs.instanceID,
		Model:              cs.model,
		Status:             cs.status,
		ExpiresAt:          cs.expiresAt,
		GracePeriodEndsAt:  cs.gracePeriodEndsAt,
		Position:           cs.position,
		QueueDepth:         cs.queueDepth,
		PollAt:             cs.pollAt,
		AccessTier:         cs.accessTier,
		CountryCode:        cs.countryCode,
		CountryBlockReason: cs.countryBlockReason,
	}
	s.flushLocked()
}

// Remove drops key from the store (session invalidated/ended at runtime).
// When expectedInstanceID is non-empty the entry is only removed if its
// stored instance id matches, so a stale invalidation cannot clobber a newer
// resumed session; an empty expectedInstanceID removes unconditionally.
func (s *Store) Remove(key, expectedInstanceID string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	ps, ok := s.data[key]
	if !ok {
		return
	}
	if expectedInstanceID != "" && ps.InstanceID != expectedInstanceID {
		return
	}
	delete(s.data, key)
	s.flushLocked()
}

// flushLocked writes the current map atomically. Caller holds s.mu.
func (s *Store) flushLocked() {
	file := storeFile{Version: storeVersion, Sessions: s.data}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		slog.Warn("session store: marshal failed", "path", s.path, "err", err)
		return
	}

	dir := filepath.Dir(s.path)
	if dir == "" {
		dir = "."
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("session store: mkdir failed", "dir", dir, "err", err)
		return
	}

	// Write a temp file in the target directory, then rename it over the
	// target so a crash mid-write never leaves a truncated state file.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp*")
	if err != nil {
		slog.Warn("session store: temp create failed", "dir", dir, "err", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		slog.Warn("session store: write failed", "path", s.path, "err", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		slog.Warn("session store: close failed", "path", s.path, "err", err)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		if _, statErr := os.Stat(s.path); statErr == nil {
			// The target exists but rename-over-existing failed (e.g.
			// Windows without MOVEFILE_REPLACE_EXISTING): fall back to
			// removing the target first, then renaming.
			_ = os.Remove(s.path)
			if err := os.Rename(tmpName, s.path); err == nil {
				return
			}
		}
		_ = os.Remove(tmpName)
		slog.Warn("session store: rename failed", "path", s.path, "err", err)
	}
}
