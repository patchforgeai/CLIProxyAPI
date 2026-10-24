package authseal

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// renameWaits are the pauses between attempts to rename over a file that another handle holds
// open (Windows refuses to replace it until that handle closes: the auth watcher reads a file
// right after it changes). About 1.7 s in all.
var renameWaits = []time.Duration{
	10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond,
	160 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond,
	200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond,
}

// replaceFile writes content to a temporary file next to path (mode 0600, synced), gives it
// path's creation time where the system allows it, and renames it over path. The temporary name
// does not end in .json, so neither the watcher nor a listing takes it for an auth file, and it
// is removed when anything fails. A path that is a symbolic link is written where the link
// points, as writing through it in place did, so the link stays.
func replaceFile(path string, content []byte) (err error) {
	if resolved, errResolve := filepath.EvalSymlinks(path); errResolve == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("authseal: create %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("authseal: create a temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err = temp.Write(content); err != nil {
		_ = temp.Close()
		return fmt.Errorf("authseal: write %s: %w", tempPath, err)
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("authseal: sync %s: %w", tempPath, err)
	}
	created, hasCreated := creationTime(path)
	if hasCreated {
		keepCreationTimeOpen(temp, created)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("authseal: close %s: %w", tempPath, err)
	}
	if err = os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("authseal: chmod %s: %w", tempPath, err)
	}
	if hasCreated {
		keepCreationTimeClosed(tempPath, created)
	}
	if err = renameOver(tempPath, path); err != nil {
		return fmt.Errorf("authseal: replace %s: %w", path, err)
	}
	return nil
}

// renameBlockedHook, when set (tests), is called each time a rename finds the file held open.
var renameBlockedHook func()

// renameOver renames from over to, again for a while when another handle holds to open.
func renameOver(from, to string) error {
	err := os.Rename(from, to)
	for _, wait := range renameWaits {
		if err == nil || !renameBlocked(err) {
			break
		}
		if renameBlockedHook != nil {
			renameBlockedHook()
		}
		time.Sleep(wait)
		err = os.Rename(from, to)
	}
	return err
}
