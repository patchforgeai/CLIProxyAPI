package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/authseal"
)

// devyre: supervisors find -auth-files-key-stdin in the help output, and it takes no value.
func TestAuthFilesKeyStdinFlag(t *testing.T) {
	f := flag.CommandLine.Lookup("auth-files-key-stdin")
	if f == nil {
		t.Fatal("-auth-files-key-stdin is not registered")
	}
	if boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !boolFlag.IsBoolFlag() {
		t.Fatal("-auth-files-key-stdin must be a boolean flag")
	}
	if name, _ := flag.UnquoteUsage(f); name != "" {
		t.Fatalf("the usage names a value (%q); the help would not list the flag alone on its line", name)
	}
	if !argvEnablesBoolFlag([]string{"-auth-files-key-stdin", "--discover-json"}, "discover-json") {
		t.Fatal("argv scanning took --discover-json as the value of -auth-files-key-stdin")
	}

	devyreAuthFilesKeyStdin = false
	if err := devyreApplyAuthFilesKeyStdin(); err != nil || authseal.Enabled() {
		t.Fatalf("without the flag: %v, enabled %v", err, authseal.Enabled())
	}
}

func TestDevyreSealAuthDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-ada@example.com.json")
	plaintext := []byte(`{"type":"claude","access_token":"secret"}`)
	if err := os.WriteFile(path, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	// Without a key nothing changes, whatever the store.
	for _, otherStore := range []bool{false, true} {
		if err := devyreSealAuthDir(dir, otherStore); err != nil {
			t.Fatalf("without a key (other store %v): %v", otherStore, err)
		}
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, plaintext) {
		t.Fatalf("without a key the file changed: %q", got)
	}

	if err := authseal.SetKey(bytes.Repeat([]byte{7}, authseal.KeySize)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authseal.SetKey(nil) })
	if err := devyreSealAuthDir(dir, true); err == nil || !strings.Contains(err.Error(), "file token store only") {
		t.Fatalf("a key with another store: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, plaintext) {
		t.Fatal("the refused start changed the file")
	}
	if err := devyreSealAuthDir(dir, false); err != nil {
		t.Fatalf("with the key: %v", err)
	}
	if got, _ := os.ReadFile(path); !authseal.IsSealed(got) {
		t.Fatalf("not sealed at start: %q", got)
	}
	if got, err := authseal.ReadFile(path); err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("reads back as %q, %v", got, err)
	}
}
