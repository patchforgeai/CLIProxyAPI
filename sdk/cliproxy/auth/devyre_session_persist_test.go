package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// fakePersistTimer is a timer the test fires by hand.
type fakePersistTimer struct {
	after   time.Duration
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakePersistTimer) Stop() bool {
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// fakePersistClock records the timers a SessionPersistence arms.
type fakePersistClock struct {
	mu     sync.Mutex
	timers []*fakePersistTimer
}

func (c *fakePersistClock) afterFunc(after time.Duration, fn func()) persistTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakePersistTimer{after: after, fn: fn}
	c.timers = append(c.timers, timer)
	return timer
}

// armed returns the timers neither fired nor stopped.
func (c *fakePersistClock) armed() []*fakePersistTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var armed []*fakePersistTimer
	for _, timer := range c.timers {
		if !timer.fired && !timer.stopped {
			armed = append(armed, timer)
		}
	}
	return armed
}

// fireOnly fires the one armed timer, which must wait the save interval.
func (c *fakePersistClock) fireOnly(t *testing.T) {
	t.Helper()
	armed := c.armed()
	if len(armed) != 1 || armed[0].after != sessionPersistInterval {
		t.Fatalf("armed timers = %+v, want one of %s", armed, sessionPersistInterval)
	}
	c.mu.Lock()
	armed[0].fired = true
	c.mu.Unlock()
	armed[0].fn()
}

func newTestPersistence(path string, clock *fakePersistClock) *SessionPersistence {
	p := NewSessionPersistence(path)
	p.afterFunc = clock.afterFunc
	return p
}

func newPersistTestSelector() *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
}

var persistTestAuths = []*Auth{{ID: "persist-auth-a", Provider: "claude"}, {ID: "persist-auth-b", Provider: "claude"}}

func persistTestPick(t *testing.T, selector *SessionAffinitySelector, session string) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts(session), persistTestAuths)
	if err != nil {
		t.Fatal(err)
	}
	return got.ID
}

// sameSessionBindings compares bindings by keys, credential and expiry instant.
func sameSessionBindings(a, b []sessionBinding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if fmt.Sprint(a[i].Keys) != fmt.Sprint(b[i].Keys) || a[i].AuthID != b[i].AuthID || !a[i].ExpiresAt.Equal(b[i].ExpiresAt) {
			return false
		}
	}
	return true
}

func readPersistTestFile(t *testing.T, path string) sessionAffinityFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved sessionAffinityFile
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved file %s is not JSON: %v", data, err)
	}
	return saved
}

func TestDevyreSessionPersistSavesAndRestores(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SessionAffinityFileName)
	clock := &fakePersistClock{}
	selector := newPersistTestSelector()
	defer selector.Stop()
	persistence := newTestPersistence(path, clock)
	defer persistence.Close()
	if restored := persistence.Load(selector); restored != 0 {
		t.Fatalf("Load() without a file = %d", restored)
	}
	persistence.Attach(selector)
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("a timer is armed while nothing changed: %+v", armed)
	}

	bound := map[string]string{"one": persistTestPick(t, selector, "one"), "two": persistTestPick(t, selector, "two")}
	selector.cache.SetAliases(bound["two"], "claude::claude:two::claude-opus-5-5", "claude::claude:two-alias::claude-opus-5-5")
	clock.fireOnly(t)
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("a timer is armed after the save: %+v", armed)
	}
	saved := readPersistTestFile(t, path)
	want := selector.cache.devyreBindings(time.Now())
	if saved.Version != 1 || len(saved.Bindings) != 2 || len(want) != 2 {
		t.Fatalf("saved = %+v, want two bindings of version 1", saved)
	}
	for i, entry := range saved.Bindings {
		if keys := append([]string{entry.Key}, entry.Aliases...); fmt.Sprint(keys) != fmt.Sprint(want[i].Keys) ||
			entry.AuthID != want[i].AuthID || !entry.ExpiresAt.Equal(want[i].ExpiresAt) {
			t.Fatalf("saved binding %d = %+v, want %+v", i, entry, want[i])
		}
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, %v; want 0600", info.Mode(), err)
		}
	}

	// A restart: a new selector restores every binding with its expiry.
	restarted := newPersistTestSelector()
	defer restarted.Stop()
	if restored := newTestPersistence(path, &fakePersistClock{}).Load(restarted); restored != 2 {
		t.Fatalf("Load() = %d, want 2", restored)
	}
	if got := restarted.cache.devyreBindings(time.Now()); !sameSessionBindings(got, want) {
		t.Fatalf("restored bindings = %+v, want %+v", got, want)
	}
	for session, authID := range bound {
		if restoredID, status := restarted.LookupAffinity("claude", "claude-opus-5-5", "claude:"+session); status != "bound" || restoredID != authID {
			t.Fatalf("session %s is %s %q after the restart, want %s", session, status, restoredID, authID)
		}
	}
}

// The save timer runs only while something changed: at most one is armed, and
// none while the bindings are saved or only read.
func TestDevyreSessionPersistNoTimerWhileClean(t *testing.T) {
	t.Parallel()

	clock := &fakePersistClock{}
	selector := newPersistTestSelector()
	defer selector.Stop()
	persistence := newTestPersistence(filepath.Join(t.TempDir(), SessionAffinityFileName), clock)
	defer persistence.Close()
	persistence.Attach(selector)
	persistence.Attach(selector)
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("armed after attaching = %+v", armed)
	}
	for _, session := range []string{"a", "b", "c"} {
		persistTestPick(t, selector, session)
	}
	if armed := clock.armed(); len(armed) != 1 {
		t.Fatalf("armed after three changes = %d timers, want one", len(armed))
	}
	clock.fireOnly(t)
	selector.LookupAffinity("claude", "claude-opus-5-5", "claude:a")
	selector.cache.Get("claude::claude:a::claude-opus-5-5")
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("armed after reads = %+v", armed)
	}
	persistTestPick(t, selector, "a") // a use refreshes the expiry: a change
	clock.fireOnly(t)
	if total := len(clock.timers); total != 2 {
		t.Fatalf("timers armed in total = %d, want 2", total)
	}
}

// Close writes what changed since the last save, stops the timer, and leaves
// nothing behind: later changes arm nothing.
func TestDevyreSessionPersistFlushesOnClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SessionAffinityFileName)
	clock := &fakePersistClock{}
	selector := newPersistTestSelector()
	defer selector.Stop()
	clean := newTestPersistence(path, clock)
	clean.Attach(selector)
	clean.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("closing without a change wrote the file: %v", err)
	}

	persistence := newTestPersistence(path, clock)
	persistence.Attach(selector)
	authID := persistTestPick(t, selector, "flushed")
	armed := clock.armed()
	if len(armed) != 1 {
		t.Fatalf("armed = %+v", armed)
	}
	persistence.Close()
	if !armed[0].stopped {
		t.Fatal("Close left the timer armed")
	}
	saved := readPersistTestFile(t, path)
	if len(saved.Bindings) != 1 || saved.Bindings[0].Key != "claude::claude:flushed::claude-opus-5-5" || saved.Bindings[0].AuthID != authID {
		t.Fatalf("saved = %+v", saved)
	}
	persistTestPick(t, selector, "after-close")
	persistence.Changed()
	persistence.Attach(selector)
	persistence.Close()
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("armed after Close = %+v", armed)
	}
}

func TestDevyreSessionPersistDropsExpiredAndMalformedEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SessionAffinityFileName)
	now := time.Now().UTC()
	entry := func(key string, aliases []string, authID string, expiresAt time.Time) sessionAffinityEntry {
		return sessionAffinityEntry{Key: key, Aliases: aliases, AuthID: authID, ExpiresAt: expiresAt}
	}
	document := sessionAffinityFile{Version: 1, Bindings: []sessionAffinityEntry{
		entry("claude::claude:kept::claude-opus-5-5", []string{"claude::claude:kept-alias::claude-opus-5-5"}, "persist-auth-a", now.Add(40*time.Minute)),
		entry("claude::claude:expired::claude-opus-5-5", nil, "persist-auth-a", now.Add(-time.Minute)),
		entry("", nil, "persist-auth-a", now.Add(time.Hour)),
		entry("claude::claude:no-auth::claude-opus-5-5", nil, "", now.Add(time.Hour)),
		entry("claude::claude:no-expiry::claude-opus-5-5", nil, "persist-auth-a", time.Time{}),
		entry("claude::claude:empty-alias::claude-opus-5-5", []string{""}, "persist-auth-a", now.Add(time.Hour)),
		// A credential that no longer exists is restored; its session fails over at its next pick.
		entry("claude::claude:gone::claude-opus-5-5", nil, "persist-auth-removed", now.Add(time.Hour)),
	}}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	selector := newPersistTestSelector()
	defer selector.Stop()
	if restored := newTestPersistence(path, &fakePersistClock{}).Load(selector); restored != 2 {
		t.Fatalf("Load() = %d, want 2", restored)
	}
	for key, want := range map[string]string{
		"claude::claude:kept::claude-opus-5-5":       "persist-auth-a",
		"claude::claude:kept-alias::claude-opus-5-5": "persist-auth-a",
		"claude::claude:gone::claude-opus-5-5":       "persist-auth-removed",
	} {
		if got, ok := selector.cache.Get(key); !ok || got != want {
			t.Fatalf("Get(%s) = %q, %v; want %s", key, got, ok, want)
		}
	}
	for _, binding := range selector.cache.devyreBindings(time.Now()) {
		if binding.Keys[0] == "claude::claude:kept::claude-opus-5-5" && !binding.ExpiresAt.Equal(now.Add(40*time.Minute)) {
			t.Fatalf("restored expiry = %v, want the saved one", binding.ExpiresAt)
		}
	}
	// The restored binding is checked like a live one: its credential is gone, so the session moves.
	if got := persistTestPick(t, selector, "gone"); got == "persist-auth-removed" {
		t.Fatal("a binding to a removed credential was used")
	}
}

// A corrupt or foreign file is logged once and ignored; a missing one is not logged.
func TestDevyreSessionPersistIgnoresCorruptAndForeignFiles(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	dir := t.TempDir()
	for name, content := range map[string]string{
		"corrupt":       "{not json",
		"array":         `[{"key":"k"}]`,
		"no-version":    `{"bindings":[{"key":"k","auth_id":"a","expires_at":"2999-01-01T00:00:00Z"}]}`,
		"other-version": `{"version":2,"bindings":[]}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		selector := newPersistTestSelector()
		hook.Reset()
		if restored := newTestPersistence(path, &fakePersistClock{}).Load(selector); restored != 0 || selector.cache.Len() != 0 {
			t.Fatalf("%s: Load() = %d, cache %d", name, restored, selector.cache.Len())
		}
		selector.Stop()
		var warnings []string
		for _, entry := range hook.AllEntries() {
			if entry.Level == log.WarnLevel {
				warnings = append(warnings, entry.Message)
			}
		}
		if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "session-affinity: ignoring "+path+": ") {
			t.Fatalf("%s: warnings = %q", name, warnings)
		}
	}
	hook.Reset()
	selector := newPersistTestSelector()
	defer selector.Stop()
	if restored := newTestPersistence(filepath.Join(dir, "missing.json"), &fakePersistClock{}).Load(selector); restored != 0 || len(hook.AllEntries()) != 0 {
		t.Fatalf("missing file: Load() = %d, logs %+v", restored, hook.AllEntries())
	}
}

// A save replaces the file whole, through a temporary file that never stays behind.
func TestDevyreSessionPersistReplacesTheFileAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SessionAffinityFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"bindings":[{"key":"old","auth_id":"old","expires_at":"2999-01-01T00:00:00Z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	selector := newPersistTestSelector()
	defer selector.Stop()
	persistTestPick(t, selector, "new")
	persistence := newTestPersistence(path, &fakePersistClock{})
	if err := persistence.save(selector.cache); err != nil {
		t.Fatal(err)
	}
	if saved := readPersistTestFile(t, path); len(saved.Bindings) != 1 || saved.Bindings[0].Key != "claude::claude:new::claude-opus-5-5" {
		t.Fatalf("saved = %+v, want only the new binding", saved)
	}
	assertOnlyFiles(t, dir, SessionAffinityFileName)

	// A rename that fails leaves the old content and no temporary file.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, SessionAffinityFileName, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := newTestPersistence(filepath.Join(blocked, SessionAffinityFileName), &fakePersistClock{}).save(selector.cache); err == nil {
		t.Fatal("saving over a folder succeeded")
	}
	assertOnlyFiles(t, blocked, SessionAffinityFileName)
}

func assertOnlyFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if fmt.Sprint(got) != fmt.Sprint(names) {
		t.Fatalf("%s holds %q, want %q", dir, got, names)
	}
}

// The file holds at most what the cache holds.
func TestDevyreSessionPersistIsBoundedByTheCache(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SessionAffinityFileName)
	selector := newPersistTestSelector()
	defer selector.Stop()
	selector.cache.Stop()
	selector.cache = NewSessionCacheWithCapacity(time.Hour, 3)
	defer selector.cache.Stop()
	for i := 0; i < 5; i++ {
		persistTestPick(t, selector, fmt.Sprintf("s%d", i))
	}
	if err := newTestPersistence(path, &fakePersistClock{}).save(selector.cache); err != nil {
		t.Fatal(err)
	}
	if saved := readPersistTestFile(t, path); len(saved.Bindings) != 3 {
		t.Fatalf("saved %d bindings, want the cache's capacity of 3", len(saved.Bindings))
	}
}

// After a selector is rebuilt, the persistence follows the new cache and the
// old one no longer schedules saves.
func TestDevyreSessionPersistFollowsTheAttachedCache(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SessionAffinityFileName)
	clock := &fakePersistClock{}
	old := newPersistTestSelector()
	defer old.Stop()
	persistence := newTestPersistence(path, clock)
	defer persistence.Close()
	persistence.Attach(old)
	persistTestPick(t, old, "carried")
	rebuilt := newPersistTestSelector()
	defer rebuilt.Stop()
	CarrySessionBindings(old, rebuilt)
	clock.fireOnly(t)

	persistence.Attach(rebuilt)
	clock.fireOnly(t) // the switch schedules a save
	persistTestPick(t, old, "stale")
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("the old cache still schedules saves: %+v", armed)
	}
	persistTestPick(t, rebuilt, "fresh")
	clock.fireOnly(t)
	saved := readPersistTestFile(t, path)
	var keys []string
	for _, entry := range saved.Bindings {
		keys = append(keys, entry.Key)
	}
	if len(keys) != 2 || strings.Contains(strings.Join(keys, ","), "stale") {
		t.Fatalf("saved keys = %q, want the carried and the fresh binding", keys)
	}
}
