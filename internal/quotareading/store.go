package quotareading

import (
	"math"
	"sort"
	"sync"
	"time"
)

// maxResetRollForward bounds how far a stale reset is rolled forward. Older
// resets are treated as unknown rather than risking duration overflow.
const maxResetRollForward = 100 * 365 * 24 * time.Hour

// Store keeps the newest observation of every window per credential. It is
// safe for concurrent use. A nil *Store behaves as an empty store.
type Store struct {
	mu      sync.RWMutex
	entries map[string]*storeEntry
}

type storeEntry struct {
	provider string
	windows  map[string]Window
}

var defaultStore = NewStore()

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{entries: make(map[string]*storeEntry)}
}

// Default returns the process-wide store.
func Default() *Store {
	return defaultStore
}

// Put records windows for authID. Per window ID, a window replaces the stored
// one only when its ObservedAt is newer or equal. Windows with an empty ID or a
// NaN usage are dropped; usage is clamped to 0..100. An empty authID is ignored.
// Put merges a partial view: it never removes a stored window. Use Replace for
// a body that lists every window the provider reports.
func (s *Store) Put(authID, provider string, ws []Window) {
	if s == nil || authID == "" || len(ws) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(authID, normalizeProvider(provider), ws)
}

// Replace records ws as the complete set of windows the provider reported for
// authID at observedAt, such as one usage body. A stored window whose ID ws
// lacks is removed unless it was observed after observedAt, so a window the
// provider stopped reporting (a plan change, or a Codex account whose
// positional windows changed meaning) cannot linger and later be rolled
// forward as a fresh full window. The windows of ws are then recorded as Put
// records them. A credential left without windows is forgotten.
func (s *Store) Replace(authID, provider string, ws []Window, observedAt time.Time) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.entries[authID]; entry != nil {
		reported := make(map[string]bool, len(ws))
		for _, window := range ws {
			if window.ID != "" && !math.IsNaN(window.UsedPercent) {
				reported[window.ID] = true
			}
		}
		for id, stored := range entry.windows {
			if !reported[id] && !stored.ObservedAt.After(observedAt) {
				delete(entry.windows, id)
			}
		}
	}
	s.putLocked(authID, normalizeProvider(provider), ws)
	if entry := s.entries[authID]; entry != nil && len(entry.windows) == 0 {
		delete(s.entries, authID)
	}
}

// putLocked implements Put. Callers hold s.mu for writing.
func (s *Store) putLocked(authID, provider string, ws []Window) {
	entry := s.entries[authID]
	for _, window := range ws {
		if window.ID == "" || math.IsNaN(window.UsedPercent) {
			continue
		}
		window.UsedPercent = clampPercent(window.UsedPercent)
		if entry == nil {
			entry = &storeEntry{windows: make(map[string]Window)}
			s.entries[authID] = entry
		}
		if stored, ok := entry.windows[window.ID]; ok && window.ObservedAt.Before(stored.ObservedAt) {
			continue
		}
		entry.windows[window.ID] = window
	}
	if entry != nil && provider != "" {
		entry.provider = provider
	}
}

// Get returns a copy of the stored reading for authID with windows sorted by
// ID. An unknown credential yields a Reading with only AuthID set.
func (s *Store) Get(authID string) Reading {
	reading := Reading{AuthID: authID}
	if s == nil {
		return reading
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry := s.entries[authID]; entry != nil {
		reading.Provider = entry.provider
		reading.Windows = entry.sortedWindows()
	}
	return reading
}

// Forget removes every reading of authID.
func (s *Store) Forget(authID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, authID)
}

// Snapshot returns copies of all readings sorted by AuthID.
func (s *Store) Snapshot() []Reading {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	readings := make([]Reading, 0, len(s.entries))
	for authID, entry := range s.entries {
		readings = append(readings, Reading{AuthID: authID, Provider: entry.provider, Windows: entry.sortedWindows()})
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].AuthID < readings[j].AuthID })
	return readings
}

func (e *storeEntry) sortedWindows() []Window {
	if len(e.windows) == 0 {
		return nil
	}
	windows := make([]Window, 0, len(e.windows))
	for _, window := range e.windows {
		windows = append(windows, window)
	}
	sortWindows(windows)
	return windows
}

// Effective merges the stored windows of authID with the windows parsed from
// its current header signals. Per window ID the newest ObservedAt wins; on a
// tie the header window wins, the same newer-or-equal rule Put applies. When
// provider is empty, the stored provider selects the signal parser.
//
// The merged windows are then normalized for now: a window whose ResetsAt is
// set and not after now has already reset, so UsedPercent becomes 0 and
// ResetsAt rolls forward by whole multiples of Length until it is after now.
// With an unknown Length the new reset time is unknown (zero).
func Effective(st *Store, authID, provider string, signals map[string]string, signalsAt, now time.Time) Reading {
	reading := st.Get(authID)
	if provider = normalizeProvider(provider); provider == "" {
		provider = reading.Provider
	}
	reading.Provider = provider
	headerWindows := FromSignals(provider, signals, signalsAt)
	if len(headerWindows) > 0 {
		byID := make(map[string]Window, len(reading.Windows)+len(headerWindows))
		for _, window := range reading.Windows {
			byID[window.ID] = window
		}
		for _, window := range headerWindows {
			if stored, ok := byID[window.ID]; ok && window.ObservedAt.Before(stored.ObservedAt) {
				continue
			}
			byID[window.ID] = window
		}
		merged := make([]Window, 0, len(byID))
		for _, window := range byID {
			merged = append(merged, window)
		}
		sortWindows(merged)
		reading.Windows = merged
	}
	for i := range reading.Windows {
		reading.Windows[i] = normalizeWindow(reading.Windows[i], now)
	}
	return reading
}

// normalizeWindow applies a reset that has already happened by now.
func normalizeWindow(window Window, now time.Time) Window {
	if window.ResetsAt.IsZero() || window.ResetsAt.After(now) {
		return window
	}
	window.UsedPercent = 0
	elapsed := now.Sub(window.ResetsAt)
	if window.Length <= 0 || elapsed >= maxResetRollForward {
		window.ResetsAt = time.Time{}
		return window
	}
	periods := elapsed/window.Length + 1
	window.ResetsAt = window.ResetsAt.Add(periods * window.Length)
	return window
}
