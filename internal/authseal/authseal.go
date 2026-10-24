// Package authseal keeps the auth files, the credentials in auth-dir, encrypted at rest while the
// server holds a key for them (-auth-files-key-stdin), and reads them either way.
//
// With a key, every auth file the server writes is sealed: AES-256-GCM with a fresh 96-bit nonce,
// the file's name as associated data, so a sealed file opens only under its own name. Without
// one, the server reads and writes plaintext JSON as upstream does. Either way a file is written
// to a temporary file beside it and renamed over it, keeping its creation time where the system
// lets a program set it (Windows, macOS), so a kill mid-write never leaves half a credential.
//
// devyre: the whole package is fork-only.
package authseal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/text/unicode/norm"
)

// KeySize is the size of a key in bytes (AES-256).
const KeySize = 32

const (
	// sealedPrefix starts every sealed auth file. A sealed file is one line:
	//
	//	cpa-sealed-auth.v1.<nonce>.<ciphertext>
	//
	// both base64url without padding: the 12-byte nonce, then the file's JSON encrypted with its
	// 16-byte tag. It is never valid JSON, so code that reads it as JSON fails instead of taking
	// it for a credential.
	sealedPrefix = "cpa-sealed-auth.v1."
	// aadContext and the file's name are the associated data.
	aadContext = "cpa-sealed-auth.v1\n"
	nonceSize  = 12
	tagSize    = 16
)

// ErrSealed is what reading a sealed auth file fails with when it cannot be opened: the server
// has no key, another key sealed it, it was changed, or it was moved to another name.
var ErrSealed = errors.New("sealed auth file cannot be opened")

// sealer holds the key's cipher.
type sealer struct {
	aead cipher.AEAD
}

var current atomic.Pointer[sealer]

// SetKey makes key the process's key for the auth files: from then on every auth file written is
// sealed with it, and sealed files are opened with it. An empty key removes it.
func SetKey(key []byte) error {
	if len(key) == 0 {
		current.Store(nil)
		return nil
	}
	if len(key) != KeySize {
		return fmt.Errorf("authseal: the key is %d bytes, not %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("authseal: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("authseal: %w", err)
	}
	current.Store(&sealer{aead: aead})
	return nil
}

// Enabled reports whether the process has a key, so auth files are written sealed.
func Enabled() bool {
	return current.Load() != nil
}

// IsSealed reports whether data is a sealed auth file (by its prefix, before any key is tried).
func IsSealed(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte(sealedPrefix))
}

// associatedData binds a sealed file to its name: the base name, in Unicode NFC and lower case,
// the forms the name can take for one file (Windows lower-cases the IDs, macOS may decompose).
func associatedData(path string) []byte {
	name := strings.ToLower(norm.NFC.String(filepath.Base(path)))
	return []byte(aadContext + name)
}

// Seal returns plaintext sealed for the auth file at path (only its name counts) with the
// process's key.
func Seal(path string, plaintext []byte) ([]byte, error) {
	s := current.Load()
	if s == nil {
		return nil, errors.New("authseal: no key to seal with")
	}
	return s.seal(path, plaintext)
}

func (s *sealer) seal(path string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("authseal: nonce: %w", err)
	}
	body := s.aead.Seal(nil, nonce, plaintext, associatedData(path))
	encoding := base64.RawURLEncoding
	out := make([]byte, 0, len(sealedPrefix)+encoding.EncodedLen(nonceSize)+1+encoding.EncodedLen(len(body))+1)
	out = append(out, sealedPrefix...)
	out = encoding.AppendEncode(out, nonce)
	out = append(out, '.')
	out = encoding.AppendEncode(out, body)
	return append(out, '\n'), nil
}

// Open returns the JSON a sealed auth file at path holds; errors wrap ErrSealed.
func Open(path string, data []byte) ([]byte, error) {
	text := bytes.TrimSpace(data)
	if !bytes.HasPrefix(text, []byte(sealedPrefix)) {
		return nil, fmt.Errorf("%w: not a sealed file", ErrSealed)
	}
	encodedNonce, encodedBody, found := bytes.Cut(text[len(sealedPrefix):], []byte{'.'})
	if !found {
		return nil, fmt.Errorf("%w: malformed", ErrSealed)
	}
	encoding := base64.RawURLEncoding
	nonce, errNonce := encoding.AppendDecode(nil, encodedNonce)
	body, errBody := encoding.AppendDecode(nil, encodedBody)
	if errNonce != nil || errBody != nil || len(nonce) != nonceSize || len(body) < tagSize {
		return nil, fmt.Errorf("%w: malformed", ErrSealed)
	}
	s := current.Load()
	if s == nil {
		return nil, fmt.Errorf("%w: the server has no key (-auth-files-key-stdin)", ErrSealed)
	}
	plaintext, err := s.aead.Open(nil, nonce, body, associatedData(path))
	if err != nil {
		return nil, fmt.Errorf("%w: another key sealed it, or it was changed or renamed", ErrSealed)
	}
	return plaintext, nil
}

// ReadFile returns the JSON of the auth file at path: as it is when the file is plaintext, opened
// when it is sealed. The errors of reading are os.ReadFile's (os.IsNotExist works on them); a
// sealed file that cannot be opened fails with an error that wraps ErrSealed.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil || !IsSealed(data) {
		return data, err
	}
	return Open(path, data)
}

// WriteFile replaces the auth file at path with data, owner-only (0600): sealed while the process
// has a key, plaintext otherwise. The new content is written to a temporary file in the same
// folder, which then takes the file's creation time where the system allows it and is renamed
// over the file, so a reader sees the old file or the new one, never part of one.
func WriteFile(path string, data []byte) error {
	content := data
	if s := current.Load(); s != nil {
		sealed, err := s.seal(path, data)
		if err != nil {
			return err
		}
		content = sealed
	}
	return replaceFile(path, content)
}

// WriteJSON encodes v as json.Encoder does (HTML escaped, a trailing newline, indented by indent
// when it is not empty) and writes it with WriteFile: the bytes the token storages wrote before.
func WriteJSON(path string, v any, indent string) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(v); err != nil {
		return err
	}
	return WriteFile(path, buf.Bytes())
}
