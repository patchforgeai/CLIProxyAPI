package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

// followingExecutor refreshes to new tokens, then, in AfterRefresh, records what the store held.
type followingExecutor struct {
	schedulerProviderTestExecutor
	store         *memoryAuthTestStore
	mu            sync.Mutex
	savedAtFollow string
	started       chan struct{}
	release       chan struct{}
}

func (e *followingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if e.started != nil {
		close(e.started)
		<-e.release
	}
	auth.Metadata["access_token"] = "rotated-access"
	auth.Metadata["refresh_token"] = "rotated-refresh"
	auth.Metadata["expired"] = time.Now().Add(8 * time.Hour).Format(time.RFC3339)
	return auth, nil
}

func (e *followingExecutor) AfterRefresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.store.mu.Lock()
	saved := e.store.auths[auth.ID]
	e.store.mu.Unlock()
	e.mu.Lock()
	if saved != nil {
		e.savedAtFollow = authRefreshToken(saved)
	}
	e.mu.Unlock()
	auth.Metadata["organization_name"] = "Looked Up"
	return auth, nil
}

func savedRefreshToken(store *memoryAuthTestStore, id string) string {
	store.mu.Lock()
	defer store.mu.Unlock()
	if saved := store.auths[id]; saved != nil {
		return authRefreshToken(saved)
	}
	return ""
}

func refreshableAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"type": "claude", "access_token": "old-access", "refresh_token": "spent-refresh",
		"expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
}

// The rotated tokens are on disk before the profile lookup starts, and what the lookup changes
// is merged after.
func TestDevyreTheRotatedTokensAreSavedBeforeTheProfileLookup(t *testing.T) {
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: store}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	manager.refreshAuth(context.Background(), "claude-a.json")
	if executor.savedAtFollow != "rotated-refresh" {
		t.Fatalf("the store held %q when the lookup began, want the rotated token", executor.savedAtFollow)
	}
	got := currentAuth(t, manager, "claude-a.json")
	if got.Metadata["organization_name"] != "Looked Up" || authRefreshToken(got) != "rotated-refresh" {
		t.Fatalf("after the lookup: %v", got.Metadata)
	}
}

// A refresh whose caller went away (a request, the loop at shutdown) still saves what it got.
func TestDevyreARefreshWhoseCallerWentAwayStillSaves(t *testing.T) {
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: store}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = manager.refreshAuthForRequest(ctx, "claude-a.json", "")
	if got := savedRefreshToken(store, "claude-a.json"); got != "rotated-refresh" {
		t.Fatalf("saved %q, want the rotated token", got)
	}
}

// Registered again while it refreshed (the watcher's remove and add of its file) and still
// holding the spent token: the new tokens go to it. Upstream dropped them, leaving the dead
// token in memory and on disk.
func TestDevyreARefreshLandsOnTheCredentialRegisteredAgain(t *testing.T) {
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: store, started: make(chan struct{}), release: make(chan struct{})}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		manager.refreshAuth(context.Background(), "claude-a.json")
		close(done)
	}()
	<-executor.started
	manager.Remove(context.Background(), "claude-a.json")
	again := refreshableAuth("claude-a.json")
	again.Metadata["note"] = "registered again"
	if _, err := manager.Register(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	close(executor.release)
	<-done
	got := currentAuth(t, manager, "claude-a.json")
	if authRefreshToken(got) != "rotated-refresh" || got.Metadata["note"] != "registered again" {
		t.Fatalf("after the refresh: %v, want the rotated tokens on the credential registered again", got.Metadata)
	}
	if saved := savedRefreshToken(store, "claude-a.json"); saved != "rotated-refresh" {
		t.Fatalf("saved %q, want the rotated token", saved)
	}
}

// Its tokens replaced while it refreshed (a new sign-in): the refresh's are not its own any more.
func TestDevyreARefreshDoesNotOverwriteANewSignIn(t *testing.T) {
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: store, started: make(chan struct{}), release: make(chan struct{})}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		manager.refreshAuth(context.Background(), "claude-a.json")
		close(done)
	}()
	<-executor.started
	signedIn := refreshableAuth("claude-a.json")
	signedIn.Metadata["access_token"], signedIn.Metadata["refresh_token"] = "new-sign-in-access", "new-sign-in-refresh"
	if _, err := manager.Update(context.Background(), signedIn); err != nil {
		t.Fatal(err)
	}
	close(executor.release)
	<-done
	if got := currentAuth(t, manager, "claude-a.json"); authRefreshToken(got) != "new-sign-in-refresh" {
		t.Fatalf("refresh token %q, want the new sign-in's", authRefreshToken(got))
	}
	if saved := savedRefreshToken(store, "claude-a.json"); saved != "new-sign-in-refresh" {
		t.Fatalf("saved %q, want the new sign-in's", saved)
	}
}
