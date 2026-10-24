package auth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authseal"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func useSealKey(t *testing.T, b byte) {
	t.Helper()
	if err := authseal.SetKey(bytes.Repeat([]byte{b}, authseal.KeySize)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authseal.SetKey(nil) })
}

func requireSealed(t *testing.T, path string) {
	t.Helper()
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !authseal.IsSealed(onDisk) || bytes.Contains(onDisk, []byte("token-")) {
		t.Fatalf("%s is not sealed: %q", filepath.Base(path), onDisk)
	}
}

// devyre: with a key the store writes every auth file sealed, a sign-in's (through its token
// storage) and a refresh's (its metadata) alike, and reads them back.
func TestDevyreFileTokenStoreSealsAuthFiles(t *testing.T) {
	useSealKey(t, 4)
	ctx := context.Background()
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	const name = "claude-ada@example.com.json"
	signedIn := &cliproxyauth.Auth{
		ID:       name,
		FileName: name,
		Provider: "claude",
		Storage:  &claude.ClaudeTokenStorage{AccessToken: "token-a1", RefreshToken: "token-r1", Email: "ada@example.com"},
		Metadata: map[string]any{"type": "claude", "email": "ada@example.com"},
	}
	path, err := store.Save(ctx, signedIn)
	if err != nil {
		t.Fatalf("Save a sign-in: %v", err)
	}
	requireSealed(t, path)

	listed, err := store.List(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = %d auths, %v", len(listed), err)
	}
	loaded := listed[0]
	if loaded.Provider != "claude" || loaded.Metadata["access_token"] != "token-a1" || loaded.Metadata["email"] != "ada@example.com" {
		t.Fatalf("listed %+v", loaded)
	}

	// A refresh rotates the tokens of the loaded auth, which holds its metadata only.
	loaded.Metadata["access_token"] = "token-a2"
	loaded.Metadata["refresh_token"] = "token-r2"
	if _, err = store.Save(ctx, loaded); err != nil {
		t.Fatalf("Save a refresh: %v", err)
	}
	requireSealed(t, path)
	if listed, _ = store.List(ctx); len(listed) != 1 || listed[0].Metadata["refresh_token"] != "token-r2" {
		t.Fatalf("after the refresh: %+v", listed)
	}

	// Nothing changed: the file is not written again.
	before, _ := os.ReadFile(path)
	if _, err = store.Save(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("an unchanged save rewrote the file")
	}

	// A file this key cannot open (another key's) is replaced by a save, not a failure.
	useSealKey(t, 5)
	other := filepath.Join(dir, "claude-bo@example.com.json")
	if err = authseal.WriteFile(other, []byte(`{"type":"claude","email":"bo@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	useSealKey(t, 4)
	replacement := &cliproxyauth.Auth{
		ID:         "claude-bo@example.com.json",
		Provider:   "claude",
		Attributes: map[string]string{cliproxyauth.AttributePath: other},
		Metadata:   map[string]any{"type": "claude", "email": "bo@example.com", "access_token": "token-b1"},
	}
	if _, err = store.Save(ctx, replacement); err != nil {
		t.Fatalf("Save over another key's file: %v", err)
	}
	if data, errRead := authseal.ReadFile(other); errRead != nil || !bytes.Contains(data, []byte("token-b1")) {
		t.Fatalf("the replaced file reads %q, %v", data, errRead)
	}

	// Without the key nothing sealed is listed, and nothing is lost.
	if err = authseal.SetKey(nil); err != nil {
		t.Fatal(err)
	}
	if listed, err = store.List(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("without the key List = %d auths, %v", len(listed), err)
	}
	requireSealed(t, path)
}
