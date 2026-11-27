package authdir

import (
	"errors"
	"testing"
	"time"
)

// One router at a time holds an auth folder: a second waits for the first to let go, and gives
// up after its wait.
func TestOneRouterAtATimeHoldsTheFolder(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := Acquire(dir, 250*time.Millisecond); !errors.Is(err, ErrHeld) {
		t.Fatalf("a second hold: %v, want ErrHeld", err)
	}
	if waited := time.Since(started); waited < 200*time.Millisecond {
		t.Fatalf("the second hold gave up after %s, before its wait", waited)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = first.Release()
	}()
	second, err := Acquire(dir, 5*time.Second)
	if err != nil {
		t.Fatalf("a hold after the first let go: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("a second release: %v", err)
	}
}
