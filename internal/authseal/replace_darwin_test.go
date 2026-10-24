package authseal

import (
	"syscall"
	"testing"
	"time"
)

// setCreationTime gives the file at path the creation time when.
func setCreationTime(t *testing.T, path string, when time.Time) {
	t.Helper()
	keepCreationTimeClosed(path, syscall.NsecToTimespec(when.UnixNano()))
}

// createdAt is the creation time of the file at path.
func createdAt(t *testing.T, path string) time.Time {
	t.Helper()
	created, ok := creationTime(path)
	if !ok {
		t.Fatalf("no creation time for %s", path)
	}
	return time.Unix(created.Sec, created.Nsec).UTC()
}
