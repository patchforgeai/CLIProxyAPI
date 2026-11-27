//go:build windows

package authdir

import "golang.org/x/sys/windows"

var unsupportedForTest error = windows.ERROR_NOT_SUPPORTED
