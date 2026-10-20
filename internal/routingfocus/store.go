// Package routingfocus keeps the focused credentials: at most one per
// provider, which the expiring-first selector uses for every request it can
// serve, so its weekly quota is burned before it resets.
//
// devyre: the whole package is fork-only.
package routingfocus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileName is the name of the focus file in the auth dir. It is not a .json
// name, because the auth watcher loads every *.json file there as a credential
// (upstream keeps its cooldown state there as .cds files for the same reason).
const FileName = "routing-focus.devyre"

// fileVersion is the version of the focus file's format.
const fileVersion = 1

// Focus is one focused credential.
type Focus struct {
	AuthID string
	// Provider is the credential's provider, normalized to lower case.
	Provider string
	// Until ends the focus; the zero time means it has no end.
	Until time.Time
	// SetAt is when the focus was set.
	SetAt time.Time
}

// Active reports whether the focus applies at now: it has no end, or its end
// is after now.
func (f Focus) Active(now time.Time) bool {
	return f.Until.IsZero() || f.Until.After(now)
}

// Store holds at most one focus per provider. It is safe for concurrent use.
// An expired focus is inert everywhere and is pruned at the next write or
// load. With a file (Load), every change is written to it atomically before it
// takes effect; without one the store works in memory only.
type Store struct {
	mu   sync.RWMutex
	path string
	// focus maps a normalized provider to its focus.
	focus map[string]Focus
	// count mirrors len(focus) for Empty, which reads it without the lock.
	count atomic.Int32
	// now is the clock that decides which focuses are expired at a write.
	now func() time.Time
}

var defaultStore = NewStore()

// NewStore returns an empty store that works in memory only.
func NewStore() *Store {
	return &Store{focus: make(map[string]Focus), now: time.Now}
}

// Default returns the process-wide store, the one the selectors and the
// management API share.
func Default() *Store {
	return defaultStore
}

// NormalizeProvider returns provider the way the store keys it: trimmed and
// lower case.
func NormalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// Load makes path the store's file and replaces the focuses in memory with
// the active ones it holds. A missing file holds none. An unreadable or
// invalid file leaves the store empty and returns the error; the path is kept,
// so the next write replaces the file. An empty path keeps the store in memory
// only, and empty.
func (s *Store) Load(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	s.replaceLocked(nil)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("routingfocus: read %s: %w", path, err)
	}
	focus, err := decodeFile(data)
	if err != nil {
		return fmt.Errorf("routingfocus: decode %s: %w", path, err)
	}
	s.replaceLocked(pruned(focus, s.now()))
	return nil
}

// Empty reports whether no focus is stored, without taking the lock, so the
// selectors' hot path costs nothing while no credential is focused. An expired
// focus counts until it is pruned: false only means a focus may apply.
func (s *Store) Empty() bool {
	return s == nil || s.count.Load() == 0
}

// Set makes f its provider's focus, replacing any other focus of that
// provider. AuthID and Provider are required, and a focus that has already
// ended is refused. A zero SetAt is set to the current time.
func (s *Store) Set(f Focus) error {
	if s == nil {
		return errors.New("routingfocus: nil store")
	}
	f.AuthID = strings.TrimSpace(f.AuthID)
	f.Provider = NormalizeProvider(f.Provider)
	if f.AuthID == "" || f.Provider == "" {
		return errors.New("routingfocus: a focus needs an auth ID and a provider")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !f.Active(now) {
		return errors.New("routingfocus: until is not in the future")
	}
	if f.SetAt.IsZero() {
		f.SetAt = now
	}
	next := pruned(s.focus, now)
	for provider, existing := range next {
		// A credential has one provider; a focus left on it under another
		// provider name would only be stale.
		if existing.AuthID == f.AuthID {
			delete(next, provider)
		}
	}
	next[f.Provider] = f
	return s.commitLocked(next)
}

// ClearProvider removes the focus of provider, if any.
func (s *Store) ClearProvider(provider string) error {
	provider = NormalizeProvider(provider)
	return s.remove(func(f Focus) bool { return f.Provider == provider })
}

// ClearAuth removes the focus of the credential authID, if any.
func (s *Store) ClearAuth(authID string) error {
	authID = strings.TrimSpace(authID)
	return s.remove(func(f Focus) bool { return f.AuthID == authID })
}

// Retain removes the focus of every credential that existing does not list.
// existing are the credential IDs as listed at listedAt; a focus set after
// listedAt is kept, because its credential may be newer than the list.
func (s *Store) Retain(existing []string, listedAt time.Time) error {
	keep := make(map[string]bool, len(existing))
	for _, authID := range existing {
		keep[authID] = true
	}
	return s.remove(func(f Focus) bool { return !keep[f.AuthID] && !f.SetAt.After(listedAt) })
}

// remove deletes every focus that drop matches, prunes the expired ones and
// writes the result when anything changed.
func (s *Store) remove(drop func(Focus) bool) error {
	if s.Empty() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := pruned(s.focus, s.now())
	for provider, f := range next {
		if drop(f) {
			delete(next, provider)
		}
	}
	if len(next) == len(s.focus) {
		return nil
	}
	return s.commitLocked(next)
}

// Focused reports whether the credential authID has an active focus at now.
func (s *Store) Focused(authID string, now time.Time) bool {
	if s.Empty() || authID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, f := range s.focus {
		if f.AuthID == authID && f.Active(now) {
			return true
		}
	}
	return false
}

// List returns the focuses active at now, sorted by provider.
func (s *Store) List(now time.Time) []Focus {
	if s.Empty() {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Focus, 0, len(s.focus))
	for _, f := range s.focus {
		if f.Active(now) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// commitLocked writes next to the file, when there is one, and then makes it
// the store's state; a failed write changes nothing. Callers hold s.mu.
func (s *Store) commitLocked(next map[string]Focus) error {
	if s.path != "" {
		if err := writeFile(s.path, next); err != nil {
			return err
		}
	}
	s.replaceLocked(next)
	return nil
}

func (s *Store) replaceLocked(focus map[string]Focus) {
	if focus == nil {
		focus = make(map[string]Focus)
	}
	s.focus = focus
	s.count.Store(int32(len(focus)))
}

// pruned returns a copy of focus without the focuses that ended by now.
func pruned(focus map[string]Focus, now time.Time) map[string]Focus {
	out := make(map[string]Focus, len(focus)+1)
	for provider, f := range focus {
		if f.Active(now) {
			out[provider] = f
		}
	}
	return out
}

// focusFile is the focus file: {"version":1,"focus":[...]}.
type focusFile struct {
	Version int         `json:"version"`
	Focus   []fileFocus `json:"focus"`
}

type fileFocus struct {
	AuthID   string `json:"auth_id"`
	Provider string `json:"provider"`
	// Until is null for a focus without an end.
	Until *time.Time `json:"until"`
	SetAt time.Time  `json:"set_at"`
}

func decodeFile(data []byte) (map[string]Focus, error) {
	var file focusFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Version != fileVersion {
		return nil, fmt.Errorf("unsupported version %d", file.Version)
	}
	focus := make(map[string]Focus, len(file.Focus))
	for _, entry := range file.Focus {
		f := Focus{AuthID: strings.TrimSpace(entry.AuthID), Provider: NormalizeProvider(entry.Provider), SetAt: entry.SetAt}
		if entry.Until != nil {
			f.Until = *entry.Until
		}
		if f.AuthID == "" || f.Provider == "" {
			return nil, errors.New("a focus without an auth ID or provider")
		}
		if _, duplicate := focus[f.Provider]; duplicate {
			return nil, fmt.Errorf("two focuses for provider %q", f.Provider)
		}
		focus[f.Provider] = f
	}
	return focus, nil
}

// writeFile replaces path atomically with focus: a temporary file in the same
// directory, mode 0600, renamed over it.
func writeFile(path string, focus map[string]Focus) error {
	file := focusFile{Version: fileVersion, Focus: make([]fileFocus, 0, len(focus))}
	for _, f := range focus {
		entry := fileFocus{AuthID: f.AuthID, Provider: f.Provider, SetAt: f.SetAt.UTC()}
		if !f.Until.IsZero() {
			until := f.Until.UTC()
			entry.Until = &until
		}
		file.Focus = append(file.Focus, entry)
	}
	sort.Slice(file.Focus, func(i, j int) bool { return file.Focus[i].Provider < file.Focus[j].Provider })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("routingfocus: encode: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("routingfocus: create %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, "."+FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("routingfocus: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err = temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("routingfocus: write %s: %w", tempPath, err)
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("routingfocus: sync %s: %w", tempPath, err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("routingfocus: close %s: %w", tempPath, err)
	}
	if err = os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("routingfocus: chmod %s: %w", tempPath, err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("routingfocus: replace %s: %w", path, err)
	}
	committed = true
	return nil
}
