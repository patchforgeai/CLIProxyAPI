//go:build !windows && !darwin

package authseal

import "os"

// Linux and the other systems give a program no way to set a file's creation time: a replaced
// auth file is as new as its last write there.
func creationTime(string) (struct{}, bool) { return struct{}{}, false }

func keepCreationTimeOpen(*os.File, struct{}) {}

func keepCreationTimeClosed(string, struct{}) {}

func renameBlocked(error) bool { return false }
