//go:build !windows && !darwin

package authseal

import (
	"testing"
	"time"
)

func setCreationTime(t *testing.T, _ string, _ time.Time) {
	t.Helper()
	t.Skip("this system lets no program set a file's creation time")
}

func createdAt(t *testing.T, _ string) time.Time {
	t.Helper()
	t.Skip("this system lets no program set a file's creation time")
	return time.Time{}
}
