// Package authdir is the router's hold on its auth folder (devyre/README.md, "No refresh is
// lost"): one router at a time refreshes the credentials of a folder. A refresh token is single
// use, so two routers on one folder (an old one still exiting while its replacement starts, which
// Anvil's supervisor does on a restart) can spend the same token, and the one that loses gets
// invalid_grant: a sign-in lost for good.
package authdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileName is the lock file in the auth folder. It does not end in .json, so neither the
// watcher nor a listing takes it for an auth file.
const FileName = ".router.lock"

// pollEvery is how often a wait for the folder tries again.
const pollEvery = 100 * time.Millisecond

// tryLockHook, when set (tests), stands in for tryLock.
var tryLockHook func(*os.File) (bool, error)

// ErrHeld: another process holds the folder.
var ErrHeld = errors.New("another router holds the auth folder")

// ErrUnsupported: the folder's filesystem takes no lock (some network and FUSE ones). A router
// then runs without one, as before there was any.
var ErrUnsupported = errors.New("the auth folder's filesystem takes no lock")

// Lock is a held auth folder.
type Lock struct {
	file *os.File
	path string
}

// Acquire holds dir for this process, waiting up to wait for another process that holds it to
// let go (or exit, which lets go for it). It fails with ErrHeld when that one still holds it, and
// with ErrUnsupported where the filesystem takes no lock.
func Acquire(dir string, wait time.Duration) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("authdir: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, FileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("authdir: open %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		lock := tryLock
		if tryLockHook != nil {
			lock = tryLockHook
		}
		held, errLock := lock(file)
		if errLock != nil {
			_ = file.Close()
			if lockUnsupported(errLock) {
				return nil, fmt.Errorf("authdir: lock %s: %w (%w)", path, ErrUnsupported, errLock)
			}
			return nil, fmt.Errorf("authdir: lock %s: %w", path, errLock)
		}
		if held {
			return &Lock{file: file, path: path}, nil
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("authdir: %s: %w", dir, ErrHeld)
		}
		time.Sleep(pollEvery)
	}
}

// Release lets go of the folder. Exiting does too.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlock(l.file)
	err := l.file.Close()
	l.file = nil
	return err
}
