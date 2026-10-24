package authseal

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// creationTime is the creation (birth) time of the file at path, when there is one.
func creationTime(path string) (syscall.Timespec, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return syscall.Timespec{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return syscall.Timespec{}, false
	}
	return stat.Birthtimespec, true
}

func keepCreationTimeOpen(*os.File, syscall.Timespec) {}

// keepCreationTimeClosed gives the temporary file the replaced file's creation time
// (setattrlist with ATTR_CMN_CRTIME): a rename keeps the renamed file's own. Best effort.
func keepCreationTimeClosed(tempPath string, created syscall.Timespec) {
	attributes := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME}
	value := unix.Timespec{Sec: created.Sec, Nsec: created.Nsec}
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&value)), unsafe.Sizeof(value))
	_ = unix.Setattrlist(tempPath, &attributes, buffer, 0)
}

func renameBlocked(error) bool { return false }
