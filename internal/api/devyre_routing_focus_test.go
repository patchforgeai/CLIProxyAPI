package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
)

// devyreLoadRoutingFocus loads routingfocus.Default() from the auth dir and
// never fails start-up. It changes the process-wide store, so it does not run
// in parallel.
func TestDevyreLoadRoutingFocus(t *testing.T) {
	t.Cleanup(func() { _ = routingfocus.Default().Load("") })

	authDir := t.TempDir()
	path := filepath.Join(authDir, routingfocus.FileName)
	saved := `{"version":1,"focus":[` +
		`{"auth_id":"claude-a.json","provider":"claude","until":null,"set_at":"2026-10-19T12:00:00Z"},` +
		`{"auth_id":"codex-a.json","provider":"codex","until":"2026-10-01T00:00:00Z","set_at":"2026-09-30T12:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	devyreLoadRoutingFocus(&config.Config{AuthDir: authDir})
	if list := routingfocus.Default().List(time.Now()); len(list) != 1 || list[0].AuthID != "claude-a.json" {
		t.Fatalf("loaded %+v, want only the focus that has not ended", list)
	}
	// Writes go to the same file.
	if err := routingfocus.Default().ClearProvider("claude"); err != nil {
		t.Fatal(err)
	}
	reloaded := routingfocus.NewStore()
	if err := reloaded.Load(path); err != nil || !reloaded.Empty() {
		t.Fatalf("the cleared focus was not saved: %v %+v", err, reloaded.List(time.Now()))
	}

	// An invalid file starts empty, whatever was focused before.
	if err := routingfocus.Default().Set(routingfocus.Focus{AuthID: "stale.json", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	devyreLoadRoutingFocus(&config.Config{AuthDir: authDir})
	if !routingfocus.Default().Empty() {
		t.Fatalf("an invalid file left %+v", routingfocus.Default().List(time.Now()))
	}
}

// A config without auth-dir (one built in code: tests, embedders) keeps the
// focus in memory: the default directory is never read or written, so a test
// never touches another deployment's focus there.
func TestDevyreLoadRoutingFocusWithoutAuthDir(t *testing.T) {
	t.Cleanup(func() { _ = routingfocus.Default().Load("") })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	defaultDir := filepath.Join(home, ".cli-proxy-api")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := `{"version":1,"focus":[{"auth_id":"claude-a.json","provider":"claude","until":null,"set_at":"2026-10-19T12:00:00Z"}]}`
	path := filepath.Join(defaultDir, routingfocus.FileName)
	if err := os.WriteFile(path, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}

	devyreLoadRoutingFocus(&config.Config{})
	if !routingfocus.Default().Empty() {
		t.Fatalf("loaded %+v from the default directory", routingfocus.Default().List(time.Now()))
	}
	if err := routingfocus.Default().Set(routingfocus.Focus{AuthID: "claude-b.json", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := routingfocus.Default().Retain([]string{"claude-c.json"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != saved {
		t.Fatalf("the default directory's file changed: %q %v", data, err)
	}
}
