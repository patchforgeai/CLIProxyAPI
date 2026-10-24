package authseal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

// SealReport is what SealDir did: the plaintext auth files it sealed, the sealed ones it found,
// and the files it left as they were (named in its log lines, never their contents).
type SealReport struct {
	Sealed      int
	AlreadyOK   int
	Unreadable  int
	NotJSON     int
	WriteFailed int
}

// SealDir seals every plaintext auth file in dir and its subfolders (the *.json files the token
// store lists) with the process's key, each replaced atomically, so the credentials left from a
// server without a key are encrypted at its first start with one and no sign-in is lost. A sealed
// file is left as it is, and so is one this key cannot open (it may be another key's: it is never
// overwritten or removed here), a file that is not JSON, a link, and one that cannot be read or
// replaced; each is logged by name. Without a key it does nothing. A missing dir is no error.
func SealDir(dir string) (SealReport, error) {
	var report SealReport
	if !Enabled() || strings.TrimSpace(dir) == "" {
		return report, nil
	}
	errWalk := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			log.Warnf("auth files: cannot read %s: %v", path, err)
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return nil
		}
		name := entry.Name()
		if !entry.Type().IsRegular() {
			log.Warnf("auth files: %s is not a regular file; left as it is", name)
			return nil
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			report.WriteFailed++
			log.Warnf("auth files: cannot read %s to seal it: %v", name, errRead)
			return nil
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		if IsSealed(data) {
			if _, errOpen := Open(path, data); errOpen != nil {
				report.Unreadable++
				log.Warnf("auth files: %s: %v; left as it is", name, errOpen)
				return nil
			}
			report.AlreadyOK++
			return nil
		}
		if !json.Valid(data) {
			report.NotJSON++
			log.Warnf("auth files: %s is not JSON; left as it is", name)
			return nil
		}
		if errWrite := WriteFile(path, data); errWrite != nil {
			report.WriteFailed++
			log.Warnf("auth files: could not seal %s: %v", name, errWrite)
			return nil
		}
		report.Sealed++
		return nil
	})
	if report.Sealed > 0 {
		log.Infof("auth files: sealed %d plaintext auth file(s) with the key", report.Sealed)
	}
	return report, errWalk
}
