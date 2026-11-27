//go:build !windows

package authdir

import "golang.org/x/sys/unix"

var unsupportedForTest error = unix.ENOLCK
