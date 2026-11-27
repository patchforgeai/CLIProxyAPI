package cliproxy

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/authdir"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// devyre: a second router on the same auth folder (an old one still exiting) waits for it, and
// does not start beside it.
func TestDevyreASecondRouterDoesNotStartBesideTheFirst(t *testing.T) {
	wait := authDirHoldWait
	authDirHoldWait = 200 * time.Millisecond
	t.Cleanup(func() { authDirHoldWait = wait })
	dir := t.TempDir()
	first := &Service{cfg: &config.Config{AuthDir: dir}}
	if err := first.devyreHoldAuthDir(); err != nil {
		t.Fatal(err)
	}
	second := &Service{cfg: &config.Config{AuthDir: dir}}
	if err := second.devyreHoldAuthDir(); !errors.Is(err, authdir.ErrHeld) {
		t.Fatalf("a second router: %v, want ErrHeld", err)
	}
	first.devyreReleaseAuthDir()
	if err := second.devyreHoldAuthDir(); err != nil {
		t.Fatalf("once the first let go: %v", err)
	}
	second.devyreReleaseAuthDir()
}

// Where the auth folder's filesystem takes no lock, the router runs without one, as before.
func TestDevyreAFolderThatTakesNoLockRunsWithoutOne(t *testing.T) {
	acquire := acquireAuthDir
	acquireAuthDir = func(string, time.Duration) (*authdir.Lock, error) {
		return nil, fmt.Errorf("authdir: lock: %w", authdir.ErrUnsupported)
	}
	t.Cleanup(func() { acquireAuthDir = acquire })
	service := &Service{cfg: &config.Config{AuthDir: t.TempDir()}}
	if err := service.devyreHoldAuthDir(); err != nil {
		t.Fatalf("a folder without locks: %v, want to run without one", err)
	}
	if service.devyreAuthDir != nil {
		t.Fatal("a hold was kept where none was taken")
	}
}
