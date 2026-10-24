package cmd

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/authseal"
)

// ReadStdinAuthFilesKey reads the key that seals the auth files (internal/authseal) from the next
// line of stdin (-auth-files-key-stdin): the line after the password under -password-stdin, the
// first one otherwise. Like the password there, it never appears on the command line or in the
// environment, and with -lifeline-stdin the rest of stdin is the lifeline.
func ReadStdinAuthFilesKey() ([]byte, error) {
	return readAuthFilesKeyLine(sharedStdin())
}

// readAuthFilesKeyLine reads a key line: 32 bytes in base64url without padding.
func readAuthFilesKeyLine(reader *bufio.Reader) ([]byte, error) {
	line, errRead := readSecretLine(reader, "key")
	if errRead != nil {
		return nil, errRead
	}
	key, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimSpace(line))
	if errDecode != nil || len(key) != authseal.KeySize {
		return nil, fmt.Errorf("the key line on stdin is not %d bytes in base64url", authseal.KeySize)
	}
	return key, nil
}
