package auth

import (
	"context"
	"testing"
	"time"
)

// The probe of 2026-11-26: StopAutoRefresh returned in 49 µs with a refresh on the wire and
// nothing saved. Now a shutdown waits for it, and its answer is saved.
func TestDevyreAShutdownWaitsForTheRefreshInFlight(t *testing.T) {
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: store, started: make(chan struct{}), release: make(chan struct{})}
	manager.RegisterExecutor(executor)
	auth := refreshableAuth("claude-a.json")
	auth.Metadata["refresh_interval_seconds"] = 1 // due at once, without a provider's lead
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	manager.StartAutoRefresh(context.Background(), 10*time.Millisecond)
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never started")
	}
	manager.StopAutoRefresh()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(executor.release)
	}()
	if left := manager.DrainRefreshes(5 * time.Second); len(left) != 0 {
		t.Fatalf("still in flight after the drain: %v", left)
	}
	if got := savedRefreshToken(store, "claude-a.json"); got != "rotated-refresh" {
		t.Fatalf("saved %q at shutdown, want the rotated token", got)
	}
}

func TestDevyreAShutdownWaitsABoundedWhile(t *testing.T) {
	store := newMemoryAuthTestStore()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &followingExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, store: store, started: make(chan struct{}), release: make(chan struct{})}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), refreshableAuth("claude-a.json")); err != nil {
		t.Fatal(err)
	}
	if left := manager.DrainRefreshes(time.Second); len(left) != 0 {
		t.Fatalf("a drain with nothing in flight found %v", left)
	}
	done := make(chan struct{})
	go func() {
		manager.refreshAuth(context.Background(), "claude-a.json")
		close(done)
	}()
	<-executor.started
	started := time.Now()
	left := manager.DrainRefreshes(30 * time.Millisecond)
	if len(left) != 1 || left[0] != "claude-a.json" {
		t.Fatalf("left in flight = %v, want the one held", left)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the drain waited %s past its bound", waited)
	}
	close(executor.release)
	<-done
}
