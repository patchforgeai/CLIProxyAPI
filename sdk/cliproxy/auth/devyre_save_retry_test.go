package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// flakyStore fails its first failures saves, then keeps what it is given.
type flakyStore struct {
	memoryAuthTestStore
	mu       sync.Mutex
	failures int
	tries    int
}

func newFlakyStore(failures int) *flakyStore {
	return &flakyStore{memoryAuthTestStore: memoryAuthTestStore{auths: make(map[string]*Auth)}, failures: failures}
}

func (s *flakyStore) Save(ctx context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.tries++
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()
	if fail {
		return "", errors.New("rename over the auth file: Access is denied")
	}
	return s.memoryAuthTestStore.Save(ctx, auth)
}

func shortSaveRetries(t *testing.T) {
	t.Helper()
	base, ceiling := saveRetryBase, saveRetryMax
	saveRetryBase, saveRetryMax = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { saveRetryBase, saveRetryMax = base, ceiling })
}

// A refresh whose save fails is saved again until it lands: before, the file kept the spent
// refresh token until the next write, four hours later at best.
func TestDevyreAFailedSaveOfRotatedTokensIsTriedUntilItLands(t *testing.T) {
	shortSaveRetries(t)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	store := newFlakyStore(0)
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: &store.memoryAuthTestStore})
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failures = 3
	store.mu.Unlock()
	manager.refreshAuth(context.Background(), "claude-a.json")

	deadline := time.Now().Add(2 * time.Second)
	for savedRefreshToken(&store.memoryAuthTestStore, "claude-a.json") != "rotated-refresh" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := savedRefreshToken(&store.memoryAuthTestStore, "claude-a.json"); got != "rotated-refresh" {
		t.Fatalf("saved %q, want the rotated token once a save landed", got)
	}
	landed := false
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.InfoLevel && strings.HasPrefix(entry.Message, "saved claude-a.json after 3 failed tries") {
			landed = true
		}
	}
	if !landed {
		t.Fatal("the save that landed was not logged")
	}
}

func TestDevyreAShutdownTriesTheUnsavedOnceMore(t *testing.T) {
	saveRetryBase, saveRetryMax = time.Hour, time.Hour
	t.Cleanup(func() { saveRetryBase, saveRetryMax = time.Second, time.Minute })
	store := newFlakyStore(0)
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: &store.memoryAuthTestStore})
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failures = 2 // the refresh's save and its lookup's
	store.mu.Unlock()
	manager.refreshAuth(context.Background(), "claude-a.json")
	if got := savedRefreshToken(&store.memoryAuthTestStore, "claude-a.json"); got == "rotated-refresh" {
		t.Fatal("the save landed though the store refused it")
	}
	if unsaved := manager.FlushPendingSaves(); len(unsaved) != 0 {
		t.Fatalf("unsaved after the flush: %v", unsaved)
	}
	if got := savedRefreshToken(&store.memoryAuthTestStore, "claude-a.json"); got != "rotated-refresh" {
		t.Fatalf("saved %q after the flush, want the rotated token", got)
	}

	store.mu.Lock()
	store.failures = 100
	store.mu.Unlock()
	manager2 := NewManager(store, &RoundRobinSelector{}, nil)
	if _, err := manager2.Register(context.Background(), refreshableAuth("claude-b.json")); err != nil {
		t.Fatal(err)
	}
	if unsaved := manager2.FlushPendingSaves(); len(unsaved) != 1 || unsaved[0] != "claude-b.json" {
		t.Fatalf("unsaved = %v, want the one the store refuses", unsaved)
	}
}

// The watermark is the last write that landed: a skipped save (the watcher) does not drop a
// refresh's save that comes after it, unless that save would put back tokens the newer state no
// longer holds (a new sign-in read from the file).
func TestDevyreTheSaveWatermarkIsTheLastWriteThatLanded(t *testing.T) {
	lock := &authPersistLock{}
	state := func(gen uint64, refresh string) *Auth {
		return &Auth{ID: "a", RegistrationEpoch: 1, Generation: gen, Metadata: map[string]any{"refresh_token": refresh}}
	}
	if !lock.admits(state(1, "t1"), false) {
		t.Fatal("the first save was refused")
	}
	lock.landed(state(1, "t1"))
	// A refresh installs gen 2 (t2); the watcher reads the file at gen 3, still with t2.
	lock.admits(state(3, "t2"), true)
	if !lock.admits(state(2, "t2"), false) {
		t.Fatal("the refresh's save was dropped behind the watcher's skipped one")
	}
	lock.landed(state(2, "t2"))
	if lock.admits(state(2, "t2"), false) || lock.admits(state(1, "t1"), false) {
		t.Fatal("a state the store already holds, or an older one, was saved again")
	}

	// A new sign-in read from the file (gen 5, t9) while an older refresh's save (gen 4, t3) waits.
	lock.admits(state(5, "t9"), true)
	if lock.admits(state(4, "t3"), false) {
		t.Fatal("an older refresh wrote its tokens over a new sign-in's")
	}
	if lock.admits(state(5, "t9"), false) != true {
		t.Fatal("the newest state cannot be saved")
	}
}

// The watcher reading back a file that still holds a refresh token this router spent (its save
// had not landed) keeps the credential's newer tokens and writes them again.
func TestDevyreAFileWithASpentTokenDoesNotPutItBack(t *testing.T) {
	saveRetryBase, saveRetryMax = time.Hour, time.Hour
	t.Cleanup(func() { saveRetryBase, saveRetryMax = time.Second, time.Minute })
	store := newFlakyStore(0)
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: &store.memoryAuthTestStore})
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failures = 2
	store.mu.Unlock()
	manager.refreshAuth(context.Background(), "claude-a.json")

	stale := refreshableAuth("claude-a.json") // the file as it still is: the spent token
	if _, err := manager.Update(WithSkipPersist(context.Background()), stale); err != nil {
		t.Fatal(err)
	}
	if got := authRefreshToken(currentAuth(t, manager, "claude-a.json")); got != "rotated-refresh" {
		t.Fatalf("in memory %q after the stale file, want the rotated token", got)
	}
	if got := savedRefreshToken(&store.memoryAuthTestStore, "claude-a.json"); got != "rotated-refresh" {
		t.Fatalf("saved %q, want the rotated token written again", got)
	}

	// A file with a token never spent here (a new sign-in) is taken as it is.
	signedIn := refreshableAuth("claude-a.json")
	signedIn.Metadata["refresh_token"] = "new-sign-in-refresh"
	if _, err := manager.Update(WithSkipPersist(context.Background()), signedIn); err != nil {
		t.Fatal(err)
	}
	if got := authRefreshToken(currentAuth(t, manager, "claude-a.json")); got != "new-sign-in-refresh" {
		t.Fatalf("in memory %q, want the new sign-in's", got)
	}
}
