package main

import (
	"errors"
	"flag"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/authseal"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cmd"
)

const authFilesKeyStdinUsage = `Read the key that encrypts the auth files from the next line of stdin (after the password line with -password-stdin); plaintext auth files are encrypted at start`

// devyreAuthFilesKeyStdin is -auth-files-key-stdin.
var devyreAuthFilesKeyStdin bool

// devyre: -auth-files-key-stdin keeps the auth files encrypted at rest with a key a supervising
// process hands over on stdin (internal/authseal, devyre/README.md). Registered here so main.go
// only needs the flag in argvFlagConsumesValue and two calls.
func init() {
	flag.BoolVar(&devyreAuthFilesKeyStdin, "auth-files-key-stdin", false, authFilesKeyStdinUsage)
}

// devyreApplyAuthFilesKeyStdin reads the key line under -auth-files-key-stdin and makes it the
// auth files' key. Called right after the password line is read.
func devyreApplyAuthFilesKeyStdin() error {
	if !devyreAuthFilesKeyStdin {
		return nil
	}
	key, errRead := cmd.ReadStdinAuthFilesKey()
	if errRead != nil {
		return errRead
	}
	return authseal.SetKey(key)
}

// devyreSealAuthDir encrypts the plaintext auth files in authDir once the key is set, before
// anything loads them. The file token store is the only one the key works with: the git, object
// and Postgres stores mirror the files elsewhere as they are. A file it cannot seal is logged and
// stays readable; only that conflict stops the server.
func devyreSealAuthDir(authDir string, otherStore bool) error {
	if !authseal.Enabled() {
		return nil
	}
	if otherStore {
		return errors.New("the auth files can be encrypted with the file token store only, not with a git, object or Postgres store")
	}
	_, errSeal := authseal.SealDir(authDir)
	return errSeal
}
