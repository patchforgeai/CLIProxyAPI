package authseal

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// setCreationTime gives the file at path the creation time when.
func setCreationTime(t *testing.T, path string, when time.Time) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	created := syscall.NsecToFiletime(when.UnixNano())
	if err = syscall.SetFileTime(syscall.Handle(file.Fd()), &created, nil, nil); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

// createdAt is the creation time of the file at path.
func createdAt(t *testing.T, path string) time.Time {
	t.Helper()
	created, ok := creationTime(path)
	if !ok {
		t.Fatalf("no creation time for %s", path)
	}
	return time.Unix(0, created.Nanoseconds()).UTC()
}
