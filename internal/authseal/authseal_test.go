package authseal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testKey returns a key made of one repeated byte.
func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, KeySize)
}

// useKey makes key the process's key for the test (nil: none) and restores none after it. The
// key is process-wide: no test of this package runs in parallel.
func useKey(t *testing.T, key []byte) {
	t.Helper()
	if err := SetKey(key); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	t.Cleanup(func() { _ = SetKey(nil) })
}

const tokenJSON = `{"type":"claude","email":"ada@example.com","access_token":"sk-ant-oat01-secret","refresh_token":"sk-ant-ort01-secret"}` + "\n"

func TestSetKey(t *testing.T) {
	useKey(t, nil)
	if Enabled() {
		t.Fatal("enabled without a key")
	}
	for _, size := range []int{1, 16, 31, 33, 64} {
		if err := SetKey(make([]byte, size)); err == nil {
			t.Fatalf("a %d-byte key was accepted", size)
		}
	}
	if Enabled() {
		t.Fatal("a refused key enabled sealing")
	}
	useKey(t, testKey(1))
	if !Enabled() {
		t.Fatal("not enabled with a key")
	}
	if err := SetKey(nil); err != nil || Enabled() {
		t.Fatalf("an empty key did not remove the key: %v", err)
	}
}

func TestSealAndOpen(t *testing.T) {
	useKey(t, testKey(1))
	sealed, err := Seal(filepath.Join("auths", "claude-ada@example.com.json"), []byte(tokenJSON))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !IsSealed(sealed) || IsSealed([]byte(tokenJSON)) {
		t.Fatal("IsSealed does not tell the sealed file from the plaintext")
	}
	if json.Valid(sealed) {
		t.Fatal("a sealed file is valid JSON: code reading it as JSON would take it for a credential")
	}
	text := string(sealed)
	if !strings.HasPrefix(text, sealedPrefix) || !strings.HasSuffix(text, "\n") || strings.Count(text, "\n") != 1 {
		t.Fatalf("not one line with the prefix: %q", text)
	}
	if strings.Contains(text, "secret") || strings.Contains(text, "ada@example.com") {
		t.Fatal("the sealed file shows its plaintext")
	}
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(text, sealedPrefix)), ".")
	if len(parts) != 2 {
		t.Fatalf("want nonce and body, got %d parts", len(parts))
	}
	nonce, _ := base64.RawURLEncoding.DecodeString(parts[0])
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if len(nonce) != nonceSize || len(body) != len(tokenJSON)+tagSize {
		t.Fatalf("nonce %d bytes, body %d bytes", len(nonce), len(body))
	}

	// Only the name counts, in any case and Unicode form, wherever the file is.
	for _, name := range []string{
		"claude-ada@example.com.json",
		filepath.Join("elsewhere", "CLAUDE-Ada@Example.com.JSON"),
	} {
		plaintext, errOpen := Open(name, sealed)
		if errOpen != nil || string(plaintext) != tokenJSON {
			t.Fatalf("Open(%q) = %q, %v", name, plaintext, errOpen)
		}
	}
	composed, _ := Seal("café.json", []byte(tokenJSON))
	if plaintext, errOpen := Open("café.json", composed); errOpen != nil || string(plaintext) != tokenJSON {
		t.Fatalf("a decomposed name does not open its composed twin: %v", errOpen)
	}

	again, _ := Seal("claude-ada@example.com.json", []byte(tokenJSON))
	if bytes.Equal(again, sealed) {
		t.Fatal("two seals of the same plaintext are equal: the nonce is not fresh")
	}
}

func TestOpenRefusesWhatItCannotTrust(t *testing.T) {
	useKey(t, testKey(1))
	const name = "codex-lin@example.com.json"
	sealed, err := Seal(name, []byte(tokenJSON))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	flip := func(index int) []byte {
		text := strings.TrimSpace(strings.TrimPrefix(string(sealed), sealedPrefix))
		nonceText, bodyText, _ := strings.Cut(text, ".")
		nonce, _ := base64.RawURLEncoding.DecodeString(nonceText)
		body, _ := base64.RawURLEncoding.DecodeString(bodyText)
		if index < 0 {
			nonce[0] ^= 0x01
		} else {
			body[index] ^= 0x01
		}
		return []byte(sealedPrefix + base64.RawURLEncoding.EncodeToString(nonce) + "." + base64.RawURLEncoding.EncodeToString(body) + "\n")
	}
	cases := []struct {
		name string
		path string
		data []byte
		key  []byte
		want string
	}{
		{name: "another key", path: name, data: sealed, key: testKey(2), want: "another key"},
		{name: "another name", path: "codex-eve@example.com.json", data: sealed, key: testKey(1), want: "renamed"},
		{name: "a changed byte of the ciphertext", path: name, data: flip(0), key: testKey(1), want: "changed"},
		{name: "a changed byte of the tag", path: name, data: flip(len(tokenJSON) + tagSize - 1), key: testKey(1), want: "changed"},
		{name: "a changed nonce", path: name, data: flip(-1), key: testKey(1), want: "changed"},
		// The newline and 8 base64 characters (6 bytes): still well formed, no longer whole.
		{name: "cut short", path: name, data: sealed[:len(sealed)-9], key: testKey(1), want: "changed"},
		{name: "no body", path: name, data: []byte(sealedPrefix + "AAAAAAAAAAAAAAAA\n"), key: testKey(1), want: "malformed"},
		{name: "not base64", path: name, data: []byte(sealedPrefix + "!!!.???\n"), key: testKey(1), want: "malformed"},
		{name: "plaintext", path: name, data: []byte(tokenJSON), key: testKey(1), want: "not a sealed file"},
		{name: "no key", path: name, data: sealed, key: nil, want: "no key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useKey(t, tc.key)
			plaintext, errOpen := Open(tc.path, tc.data)
			if !errors.Is(errOpen, ErrSealed) || !strings.Contains(errOpen.Error(), tc.want) || plaintext != nil {
				t.Fatalf("Open = %q, %v; want ErrSealed with %q", plaintext, errOpen, tc.want)
			}
			if strings.Contains(errOpen.Error(), "secret") {
				t.Fatal("the error shows the plaintext")
			}
		})
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.json")
	if err := os.WriteFile(plain, []byte(tokenJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	useKey(t, testKey(1))
	sealedPath := filepath.Join(dir, "sealed.json")
	if err := WriteFile(sealedPath, []byte(tokenJSON)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// With the key: plaintext as it is, a sealed file opened.
	for _, path := range []string{plain, sealedPath} {
		if data, err := ReadFile(path); err != nil || string(data) != tokenJSON {
			t.Fatalf("ReadFile(%s) = %q, %v", filepath.Base(path), data, err)
		}
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Fatalf("a missing file: %v, want not-exist", err)
	}

	// Without it: plaintext still reads, a sealed file does not.
	useKey(t, nil)
	if data, err := ReadFile(plain); err != nil || string(data) != tokenJSON {
		t.Fatalf("plaintext without a key = %q, %v", data, err)
	}
	if data, err := ReadFile(sealedPath); !errors.Is(err, ErrSealed) || data != nil {
		t.Fatalf("a sealed file without a key = %q, %v; want ErrSealed", data, err)
	}
	// A sealed file moved to another name does not open under it.
	useKey(t, testKey(1))
	moved := filepath.Join(dir, "moved.json")
	if err := os.Rename(sealedPath, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(moved); !errors.Is(err, ErrSealed) {
		t.Fatalf("a sealed file under another name: %v, want ErrSealed", err)
	}
}

// tempLeftovers lists what WriteFile may have left in dir besides the auth files.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			left = append(left, entry.Name())
		}
	}
	return left
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-ada@example.com.json")

	useKey(t, testKey(1))
	if err := WriteFile(path, []byte(tokenJSON)); err != nil {
		t.Fatalf("WriteFile with a key: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	if !IsSealed(onDisk) || bytes.Contains(onDisk, []byte("secret")) {
		t.Fatalf("written with a key but not sealed: %q", onDisk)
	}
	if data, err := ReadFile(path); err != nil || string(data) != tokenJSON {
		t.Fatalf("read back = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", info.Mode().Perm())
		}
	}

	useKey(t, nil)
	if err := WriteFile(path, []byte(tokenJSON)); err != nil {
		t.Fatalf("WriteFile without a key: %v", err)
	}
	if onDisk, _ := os.ReadFile(path); string(onDisk) != tokenJSON {
		t.Fatalf("without a key the file is %q, want the plaintext as it was given", onDisk)
	}
	if left := tempLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// A file that cannot be replaced leaves no temporary file either. (Windows refuses it as it
	// refuses a file held open: no need to wait through the attempts.)
	savedWaits := renameWaits
	renameWaits = nil
	t.Cleanup(func() { renameWaits = savedWaits })
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(blocked, []byte(tokenJSON)); err == nil {
		t.Fatal("replacing a folder succeeded")
	}
	if left := tempLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("temporary files left after a failure: %v", left)
	}
}

// pastCreation is a creation time no test file has by itself.
var pastCreation = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// The server orders accounts first seen together by their token files' creation time: a
// replaced file keeps it where the system lets a program set it.
func TestWriteFileKeepsTheCreationTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-ada@example.com.json")
	if err := os.WriteFile(path, []byte(tokenJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	setCreationTime(t, path, pastCreation)
	if got := createdAt(t, path); !got.Equal(pastCreation) {
		t.Fatalf("the test could not set the creation time: %v", got)
	}
	for _, key := range [][]byte{testKey(1), nil} {
		useKey(t, key)
		if err := WriteFile(path, []byte(tokenJSON)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got := createdAt(t, path); !got.Equal(pastCreation) {
			t.Fatalf("with key %v the creation time became %v", key != nil, got)
		}
	}
}

// Windows refuses to rename over a file another handle holds open without sharing its deletion,
// as os.Open does (the watcher reads a file as it changes): the write tries again until it is
// closed. Elsewhere the rename does not wait.
func TestWriteFileWhileAReaderHoldsTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-lin@example.com.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	blocked := 0
	renameBlockedHook = func() {
		blocked++
		if blocked == 1 {
			_ = reader.Close()
		}
	}
	t.Cleanup(func() { renameBlockedHook = nil })
	useKey(t, testKey(1))
	if err := WriteFile(path, []byte(tokenJSON)); err != nil {
		t.Fatalf("WriteFile while a reader holds the file: %v", err)
	}
	_ = reader.Close()
	if want := map[bool]int{true: 1, false: 0}[runtime.GOOS == "windows"]; blocked != want {
		t.Fatalf("the rename was blocked %d times, want %d", blocked, want)
	}
	if data, err := ReadFile(path); err != nil || string(data) != tokenJSON {
		t.Fatalf("read back = %q, %v", data, err)
	}
	if left := tempLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestWriteFileThroughALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude-ada@example.com.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	useKey(t, nil)
	if err := WriteFile(link, []byte(tokenJSON)); err != nil {
		t.Fatalf("WriteFile through a link: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != tokenJSON {
		t.Fatalf("the target holds %q", data)
	}
}

func TestWriteJSONWritesWhatTheEncoderWrote(t *testing.T) {
	value := map[string]any{"type": "claude", "email": "a<b>&c@example.com", "expired": "2099-01-01T00:00:00Z"}
	for _, indent := range []string{"", "  "} {
		var want bytes.Buffer
		encoder := json.NewEncoder(&want)
		if indent != "" {
			encoder.SetIndent("", indent)
		}
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "token.json")
		useKey(t, nil)
		if err := WriteJSON(path, value, indent); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("indent %q: wrote %q, want %q", indent, got, want.Bytes())
		}
	}
}
