package authseal

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// creationTime is the creation time of the file at path, when there is one.
func creationTime(path string) (syscall.Filetime, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return syscall.Filetime{}, false
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return syscall.Filetime{}, false
	}
	return data.CreationTime, true
}

// keepCreationTimeOpen gives the open temporary file the replaced file's creation time: a rename
// over a file takes the renamed file's own (NTFS keeps no tunnel for it). Best effort.
func keepCreationTimeOpen(temp *os.File, created syscall.Filetime) {
	_ = syscall.SetFileTime(syscall.Handle(temp.Fd()), &created, nil, nil)
}

func keepCreationTimeClosed(string, syscall.Filetime) {}

// renameBlocked reports whether a rename failed because another handle holds the file open
// without sharing its deletion (Go's own os.Open among them): worth trying again in a moment.
func renameBlocked(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
