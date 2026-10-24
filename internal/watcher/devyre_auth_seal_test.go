package watcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authseal"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// useAuthSealKey seals the auth files of the test with a key of one repeated byte (process-wide:
// these tests do not run in parallel) and removes it after the test.
func useAuthSealKey(t *testing.T, b byte) {
	t.Helper()
	if err := authseal.SetKey(bytes.Repeat([]byte{b}, authseal.KeySize)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authseal.SetKey(nil) })
}

const sealedTestAuth = `{"type":"claude","email":"ada@example.com","access_token":"sk-ant-oat01-not-real"}`

func readSealedTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// devyre: the watcher loads a sealed auth file from its plaintext, hashes the plaintext (a sealed
// rewrite of the same JSON, with a fresh nonce, is no change), and leaves a file it cannot open
// as it was loaded.
func TestDevyreWatcherReadsSealedAuthFiles(t *testing.T) {
	useAuthSealKey(t, 1)
	authDir := t.TempDir()
	path := filepath.Join(authDir, "claude-ada@example.com.json")
	if err := authseal.WriteFile(path, []byte(sealedTestAuth)); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{authDir: authDir, lastAuthHashes: make(map[string]string)}
	w.SetConfig(&config.Config{AuthDir: authDir})

	w.addOrUpdateClient(path)
	normalized := w.normalizeAuthPath(path)
	sum := sha256.Sum256([]byte(sealedTestAuth))
	if got := w.lastAuthHashes[normalized]; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash %q, want the plaintext's", got)
	}
	loaded := w.currentAuths["claude-ada@example.com.json"]
	if loaded == nil || loaded.Provider != "claude" || loaded.Metadata["email"] != "ada@example.com" {
		t.Fatalf("loaded %+v", loaded)
	}

	// The same JSON sealed again: other bytes on disk, the same file to the watcher.
	before := readSealedTestFile(t, path)
	if err := authseal.WriteFile(path, []byte(sealedTestAuth)); err != nil {
		t.Fatal(err)
	}
	resealed := readSealedTestFile(t, path)
	if bytes.Equal(before, resealed) {
		t.Fatal("a second seal wrote the same bytes")
	}
	if unchanged, err := w.authFileUnchanged(path); err != nil || !unchanged {
		t.Fatalf("a sealed rewrite of the same JSON counts as a change: %v, %v", unchanged, err)
	}

	// Changed on disk (a character in the middle of its ciphertext): not loaded, the loaded auth
	// stays.
	tampered := bytes.Clone(resealed)
	middle := len(tampered) / 2
	if tampered[middle] == 'A' {
		tampered[middle] = 'B'
	} else {
		tampered[middle] = 'A'
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := authseal.ReadFile(path); err == nil {
		t.Fatal("the tampered file opened")
	}
	w.addOrUpdateClient(path)
	if w.currentAuths["claude-ada@example.com.json"] != loaded || w.lastAuthHashes[normalized] != hex.EncodeToString(sum[:]) {
		t.Fatal("a file that does not open replaced the loaded auth")
	}

	// Without the key a sealed file is never loaded.
	other := filepath.Join(authDir, "claude-bo@example.com.json")
	if err := authseal.WriteFile(other, []byte(sealedTestAuth)); err != nil {
		t.Fatal(err)
	}
	if err := authseal.SetKey(nil); err != nil {
		t.Fatal(err)
	}
	w.addOrUpdateClient(other)
	if _, ok := w.currentAuths["claude-bo@example.com.json"]; ok {
		t.Fatal("a sealed file was loaded without the key")
	}
}

// devyre: a remove right after another one (an atomic replace raises one on Windows) is a
// duplicate only while the file is there: one that really removed it removes its auth.
func TestDevyreWatcherRemoveRightAfterAReplace(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "claude-ada@example.com.json")
	if err := os.WriteFile(path, []byte(sealedTestAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{authDir: authDir, lastAuthHashes: make(map[string]string)}
	w.SetConfig(&config.Config{AuthDir: authDir})
	w.addOrUpdateClient(path)
	normalized := w.normalizeAuthPath(path)

	// The file is there: the second remove is the replace's duplicate.
	w.lastRemoveTimes = map[string]time.Time{normalized: time.Now()}
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Remove})
	if !w.isKnownAuthFile(path) || w.currentAuths["claude-ada@example.com.json"] == nil {
		t.Fatal("a duplicate remove of a file that is there removed its auth")
	}

	// It is gone: removed, though a remove came a moment before.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.lastRemoveTimes[normalized] = time.Now()
	w.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Remove})
	if w.isKnownAuthFile(path) || w.currentAuths["claude-ada@example.com.json"] != nil {
		t.Fatal("a removed file kept its auth because a remove came just before")
	}
}

// waitForAuthUpdate reads queue until match accepts an update, failing after 10 s.
func waitForAuthUpdate(t *testing.T, queue <-chan AuthUpdate, what string, match func(AuthUpdate) bool) AuthUpdate {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case update := <-queue:
			if match(update) {
				return update
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// devyre: through real file events, as the router runs: sealed files written atomically (a
// temporary file renamed over the auth file) are added, changed and removed.
func TestDevyreWatcherFollowsSealedAuthFiles(t *testing.T) {
	useAuthSealKey(t, 2)
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+authDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(configPath, authDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.SetConfig(&config.Config{AuthDir: authDir})
	queue := make(chan AuthUpdate, 64)
	w.SetAuthUpdateQueue(queue)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = w.Stop()
	})
	if err = w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	const id = "claude-ada@example.com.json"
	path := filepath.Join(authDir, id)
	if err = authseal.WriteFile(path, []byte(sealedTestAuth)); err != nil {
		t.Fatal(err)
	}
	added := waitForAuthUpdate(t, queue, "the sealed file added", func(u AuthUpdate) bool {
		return u.ID == id && u.Action == AuthUpdateActionAdd
	})
	if added.Auth == nil || added.Auth.Metadata["email"] != "ada@example.com" {
		t.Fatalf("added %+v", added.Auth)
	}

	changed := `{"type":"claude","email":"ada@example.com","access_token":"sk-ant-oat01-refreshed","disabled":true}`
	if err = authseal.WriteFile(path, []byte(changed)); err != nil {
		t.Fatal(err)
	}
	modified := waitForAuthUpdate(t, queue, "the sealed file changed", func(u AuthUpdate) bool {
		return u.ID == id && u.Action == AuthUpdateActionModify
	})
	if modified.Auth == nil || !modified.Auth.Disabled || modified.Auth.Metadata["access_token"] != "sk-ant-oat01-refreshed" {
		t.Fatalf("modified %+v", modified.Auth)
	}

	// On Windows the watcher may still hold the file it just read (a rename can raise more than
	// one event): a remove is refused until it lets go, as the app's own removals are retried.
	for attempt := 0; ; attempt++ {
		if err = os.Remove(path); err == nil {
			break
		}
		if attempt == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitForAuthUpdate(t, queue, "the sealed file removed", func(u AuthUpdate) bool {
		return u.ID == id && u.Action == AuthUpdateActionDelete
	})
}
