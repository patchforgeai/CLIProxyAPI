package helps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// fakeContinuityTimer is a timer the test fires by hand.
type fakeContinuityTimer struct {
	after   time.Duration
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakeContinuityTimer) Stop() bool {
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// fakeContinuityClock records the timers a ClaudeContinuityPersistence arms.
type fakeContinuityClock struct {
	mu     sync.Mutex
	timers []*fakeContinuityTimer
}

func (c *fakeContinuityClock) afterFunc(after time.Duration, fn func()) continuityTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeContinuityTimer{after: after, fn: fn}
	c.timers = append(c.timers, timer)
	return timer
}

// armed returns the timers neither fired nor stopped.
func (c *fakeContinuityClock) armed() []*fakeContinuityTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var armed []*fakeContinuityTimer
	for _, timer := range c.timers {
		if !timer.fired && !timer.stopped {
			armed = append(armed, timer)
		}
	}
	return armed
}

// fireOnly fires the one armed timer, which must wait the save interval.
func (c *fakeContinuityClock) fireOnly(t *testing.T) {
	t.Helper()
	armed := c.armed()
	if len(armed) != 1 || armed[0].after != claudeContinuityPersistInterval {
		t.Fatalf("armed timers = %+v, want one of %s", armed, claudeContinuityPersistInterval)
	}
	c.mu.Lock()
	armed[0].fired = true
	c.mu.Unlock()
	armed[0].fn()
}

func newTestContinuityPersistence(path string, clock *fakeContinuityClock) *ClaudeContinuityPersistence {
	p := NewClaudeContinuityPersistence(path)
	p.afterFunc = clock.afterFunc
	return p
}

// resetContinuityForTest empties the continuity, as a new process starts, and
// detaches whatever persistence a test left attached.
func resetContinuityForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		resetClaudeDiagnosticsForTest()
		claudeDiagnosticsState.Lock()
		claudeDiagnosticsState.devyrePersistence = nil
		claudeDiagnosticsState.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func readContinuityTestFile(t *testing.T, path string) claudeContinuityFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved claudeContinuityFile
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

// continuityTestKey is the key BeginClaudeContinuity gives a conversation.
func continuityTestKey(conversation string) string {
	key, _, _, _, _ := BeginClaudeContinuity("id:cred-continuity", conversation, true, "")
	return key
}

// A conversation's pin, prompt id and previous message and request survive a
// restart, and a restored conversation takes the new process's commits.
func TestDevyreClaudeContinuitySavesAndRestores(t *testing.T) {
	resetContinuityForTest(t)
	path := filepath.Join(t.TempDir(), ClaudeContinuityFileName)
	clock := &fakeContinuityClock{}
	persistence := newTestContinuityPersistence(path, clock)
	persistence.Attach()

	key, sequence, _, _, promptID := BeginClaudeContinuity("id:cred-continuity", "conversation-1", true, "")
	if got := PinClaudeSessionDate(key, "2026-10-19"); got != "2026-10-19" {
		t.Fatalf("first pin = %q", got)
	}
	CommitClaudeContinuity(key, sequence, "msg_01before", "req_01before", promptID)
	clock.fireOnly(t)
	saved := readContinuityTestFile(t, path)
	if len(saved.Conversations) != 1 {
		t.Fatalf("saved %d conversations, want 1: %+v", len(saved.Conversations), saved)
	}
	entry := saved.Conversations[0]
	if entry.Key != key || entry.PinnedDate != "2026-10-19" || entry.PromptID != promptID ||
		entry.PreviousMessageID != "msg_01before" || entry.PreviousRequestID != "req_01before" || entry.ExpiresAt.IsZero() {
		t.Fatalf("saved entry = %+v", entry)
	}
	persistence.Close()

	// A new process, on the next day.
	resetClaudeDiagnosticsForTest()
	restarted := newTestContinuityPersistence(path, &fakeContinuityClock{})
	if restored := restarted.Load(); restored != 1 {
		t.Fatalf("Load() = %d, want 1", restored)
	}
	restarted.Attach()
	defer restarted.Close()
	if got := PinClaudeSessionDate(key, "2026-10-20"); got != "2026-10-19" {
		t.Fatalf("after a restart the conversation is pinned to %q, want its first day 2026-10-19", got)
	}
	again, nextSequence, previousMessage, previousRequest, nextPrompt := BeginClaudeContinuity("id:cred-continuity", "conversation-1", false, "")
	if again != key || previousMessage != "msg_01before" || previousRequest != "req_01before" || nextPrompt != promptID {
		t.Fatalf("after a restart Begin = %s %q %q %q, want the saved continuity", again, previousMessage, previousRequest, nextPrompt)
	}
	CommitClaudeContinuity(key, nextSequence, "msg_02after", "req_02after", nextPrompt)
	if _, _, previousMessage, _, _ = BeginClaudeContinuity("id:cred-continuity", "conversation-1", false, ""); previousMessage != "msg_02after" {
		t.Fatalf("a commit after the restart did not apply: previous message %q", previousMessage)
	}
}

// Nothing arms a timer while detached or after a save with no change since.
func TestDevyreClaudeContinuityNoTimerWhileClean(t *testing.T) {
	resetContinuityForTest(t)
	path := filepath.Join(t.TempDir(), ClaudeContinuityFileName)
	clock := &fakeContinuityClock{}
	persistence := newTestContinuityPersistence(path, clock)
	continuityTestKey("detached")
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("a detached persistence armed %d timers", len(armed))
	}
	persistence.Attach()
	continuityTestKey("first")
	continuityTestKey("second")
	clock.fireOnly(t) // two changes, one save
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("a save with nothing changed since left %d timers armed", len(armed))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	persistence.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Close saved with nothing changed: %v", err)
	}
	continuityTestKey("after-close")
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("a closed persistence armed %d timers", len(armed))
	}
}

// Close stops the armed timer and saves what it would have.
func TestDevyreClaudeContinuityFlushesOnClose(t *testing.T) {
	resetContinuityForTest(t)
	path := filepath.Join(t.TempDir(), ClaudeContinuityFileName)
	clock := &fakeContinuityClock{}
	persistence := newTestContinuityPersistence(path, clock)
	persistence.Attach()
	key := continuityTestKey("flushed")
	PinClaudeSessionDate(key, "2026-10-19")
	persistence.Close()
	persistence.Close()
	if armed := clock.armed(); len(armed) != 0 {
		t.Fatalf("Close left %d timers armed", len(armed))
	}
	if saved := readContinuityTestFile(t, path); len(saved.Conversations) != 1 || saved.Conversations[0].PinnedDate != "2026-10-19" {
		t.Fatalf("Close saved %+v", saved)
	}
}

// Expired, malformed and repeated entries are dropped; the rest is restored.
func TestDevyreClaudeContinuityDropsExpiredAndMalformedEntries(t *testing.T) {
	resetContinuityForTest(t)
	path := filepath.Join(t.TempDir(), ClaudeContinuityFileName)
	later := time.Now().Add(30 * time.Minute).UTC()
	key := func(n int) string { return fmt.Sprintf("%064x", n) }
	file := claudeContinuityFile{Version: claudeContinuityFileVersion, Conversations: []claudeContinuityFileEntry{
		{Key: key(1), PinnedDate: "2026-10-19", ExpiresAt: later},
		{Key: key(2), PinnedDate: "2026-10-19", ExpiresAt: time.Now().Add(-time.Minute).UTC()},
		{Key: "not-a-digest", PinnedDate: "2026-10-19", ExpiresAt: later},
		{Key: key(3), PinnedDate: "19.10.2026", ExpiresAt: later},
		{Key: key(4), PinnedDate: "2026-02-30", ExpiresAt: later},
		{Key: key(5), PromptID: "not-a-uuid", ExpiresAt: later},
		{Key: key(6), PreviousRequestID: "request 6", ExpiresAt: later},
		{Key: key(7), PreviousMessageID: strings.Repeat("m", claudeContinuityMaxMessageID+1), ExpiresAt: later},
		{Key: key(8), PinnedDate: "2026-10-19"},
		{Key: key(9), PinnedDate: "2026-10-18", ExpiresAt: later},
		{Key: key(9), PinnedDate: "2026-10-19", ExpiresAt: later},
	}}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if restored := newTestContinuityPersistence(path, &fakeContinuityClock{}).Load(); restored != 2 {
		t.Fatalf("Load() = %d, want the valid conversation and the later copy of the repeated one", restored)
	}
	if got := PinClaudeSessionDate(key(1), "2026-10-20"); got != "2026-10-19" {
		t.Fatalf("valid entry pin = %q", got)
	}
	if got := PinClaudeSessionDate(key(9), "2026-10-20"); got != "2026-10-19" {
		t.Fatalf("repeated key pin = %q, want its later copy", got)
	}
	for _, dropped := range []int{2, 3, 4, 5, 6, 7, 8} {
		if got := PinClaudeSessionDate(key(dropped), "2026-10-20"); got != "2026-10-20" {
			t.Fatalf("entry %d was restored: pin %q", dropped, got)
		}
	}
}

// A corrupt or foreign file is logged once and ignored; a missing one is not logged.
func TestDevyreClaudeContinuityIgnoresCorruptAndForeignFiles(t *testing.T) {
	resetContinuityForTest(t)
	oldLevel := log.GetLevel()
	savedHooks := make(log.LevelHooks)
	for level, hooks := range log.StandardLogger().Hooks {
		savedHooks[level] = append([]log.Hook(nil), hooks...)
	}
	hook := new(logtest.Hook)
	log.AddHook(hook)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(savedHooks)
	})
	dir := t.TempDir()
	for name, content := range map[string]string{
		"corrupt":       "{not json",
		"array":         `[{"key":"k"}]`,
		"no-version":    `{"conversations":[]}`,
		"other-version": `{"version":2,"conversations":[]}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		hook.Reset()
		if restored := newTestContinuityPersistence(path, &fakeContinuityClock{}).Load(); restored != 0 {
			t.Fatalf("%s: Load() = %d", name, restored)
		}
		var warnings []string
		for _, entry := range hook.AllEntries() {
			if entry.Level == log.WarnLevel {
				warnings = append(warnings, entry.Message)
			}
		}
		if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "claude-continuity: ignoring "+path+": ") {
			t.Fatalf("%s: warnings = %q", name, warnings)
		}
	}
	hook.Reset()
	if restored := newTestContinuityPersistence(filepath.Join(dir, "missing.json"), &fakeContinuityClock{}).Load(); restored != 0 || len(hook.AllEntries()) != 0 {
		t.Fatalf("missing file: Load() = %d, logs %+v", restored, hook.AllEntries())
	}
}

// A conversation already live keeps its own continuity.
func TestDevyreClaudeContinuityLiveEntriesWin(t *testing.T) {
	resetContinuityForTest(t)
	path := filepath.Join(t.TempDir(), ClaudeContinuityFileName)
	key := continuityTestKey("live")
	PinClaudeSessionDate(key, "2026-10-19")
	if err := newTestContinuityPersistence(path, &fakeContinuityClock{}).save(); err != nil {
		t.Fatal(err)
	}
	resetClaudeDiagnosticsForTest()
	if again := continuityTestKey("live"); again != key {
		t.Fatalf("the key changed: %s -> %s", key, again)
	}
	PinClaudeSessionDate(key, "2026-10-21")
	if restored := newTestContinuityPersistence(path, &fakeContinuityClock{}).Load(); restored != 0 {
		t.Fatalf("Load() = %d over a live conversation", restored)
	}
	if got := PinClaudeSessionDate(key, "2026-10-22"); got != "2026-10-21" {
		t.Fatalf("live pin = %q, want its own 2026-10-21", got)
	}
}

// The file and a restore hold at most the state's capacity, the most recent kept.
func TestDevyreClaudeContinuityIsBounded(t *testing.T) {
	resetContinuityForTest(t)
	path := filepath.Join(t.TempDir(), ClaudeContinuityFileName)
	later := time.Now().Add(30 * time.Minute).UTC()
	file := claudeContinuityFile{Version: claudeContinuityFileVersion}
	for i := 0; i < claudeDiagnosticsMaxEntries+5; i++ {
		file.Conversations = append(file.Conversations, claudeContinuityFileEntry{Key: fmt.Sprintf("%064x", i), PinnedDate: "2026-10-19", ExpiresAt: later})
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if restored := newTestContinuityPersistence(path, &fakeContinuityClock{}).Load(); restored != claudeDiagnosticsMaxEntries {
		t.Fatalf("Load() = %d, want the capacity %d", restored, claudeDiagnosticsMaxEntries)
	}
	if got := PinClaudeSessionDate(fmt.Sprintf("%064x", 0), "2026-10-20"); got != "2026-10-20" {
		t.Fatal("the least recently used conversation was restored over the capacity")
	}
	if got := PinClaudeSessionDate(fmt.Sprintf("%064x", claudeDiagnosticsMaxEntries+4), "2026-10-20"); got != "2026-10-19" {
		t.Fatal("the most recently used conversation was not restored")
	}
	if err = newTestContinuityPersistence(path, &fakeContinuityClock{}).save(); err != nil {
		t.Fatal(err)
	}
	if saved := readContinuityTestFile(t, path); len(saved.Conversations) > claudeDiagnosticsMaxEntries {
		t.Fatalf("saved %d conversations, over the capacity %d", len(saved.Conversations), claudeDiagnosticsMaxEntries)
	}
}

// A save replaces the file whole, through a temporary file that never stays behind.
func TestDevyreClaudeContinuityReplacesTheFileAtomically(t *testing.T) {
	resetContinuityForTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ClaudeContinuityFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"conversations":[{"key":"`+fmt.Sprintf("%064x", 1)+`","expires_at":"2999-01-01T00:00:00Z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key := continuityTestKey("new")
	if err := newTestContinuityPersistence(path, &fakeContinuityClock{}).save(); err != nil {
		t.Fatal(err)
	}
	if saved := readContinuityTestFile(t, path); len(saved.Conversations) != 1 || saved.Conversations[0].Key != key {
		t.Fatalf("saved = %+v, want only the live conversation", saved)
	}
	assertOnlyContinuityFiles(t, dir, ClaudeContinuityFileName)

	// A rename that fails leaves no temporary file.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, ClaudeContinuityFileName, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := newTestContinuityPersistence(filepath.Join(blocked, ClaudeContinuityFileName), &fakeContinuityClock{}).save(); err == nil {
		t.Fatal("saving over a folder succeeded")
	}
	assertOnlyContinuityFiles(t, blocked, ClaudeContinuityFileName)
}

func assertOnlyContinuityFiles(t *testing.T, dir string, names ...string) {
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

// A persistence attached after another takes its place; closing the first then
// leaves the second attached.
func TestDevyreClaudeContinuityAttachReplacesThePrevious(t *testing.T) {
	resetContinuityForTest(t)
	dir := t.TempDir()
	firstClock, secondClock := &fakeContinuityClock{}, &fakeContinuityClock{}
	first := newTestContinuityPersistence(filepath.Join(dir, "first.json"), firstClock)
	second := newTestContinuityPersistence(filepath.Join(dir, "second.json"), secondClock)
	first.Attach()
	second.Attach()
	first.Close()
	continuityTestKey("after-the-switch")
	if armed := firstClock.armed(); len(armed) != 0 {
		t.Fatalf("the replaced persistence armed %d timers", len(armed))
	}
	secondClock.fireOnly(t)
	second.Close()
	if saved := readContinuityTestFile(t, filepath.Join(dir, "second.json")); len(saved.Conversations) != 1 {
		t.Fatalf("the attached persistence saved %+v", saved)
	}
}
