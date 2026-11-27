package cliproxy

import (
	"errors"
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
