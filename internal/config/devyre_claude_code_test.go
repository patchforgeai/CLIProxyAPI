package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// claude-code.mcp-alias-secret: upstream.claude in the v8 layout, kept by
// saves, and never in the management API's JSON view of the config.
func TestDevyreClaudeMCPAliasSecretConfig(t *testing.T) {
	field, ok := reflect.TypeOf(ClaudeCodeConfig{}).FieldByName("MCPAliasSecret")
	if !ok || field.Tag.Get("yaml") != "mcp-alias-secret,omitempty" || field.Tag.Get("json") != "-" {
		t.Fatalf("ClaudeCodeConfig.MCPAliasSecret tags = %q", field.Tag)
	}

	const secret = "per-install-alias-secret-0123456789"
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver:\n  port: 8317\nupstream:\n  claude:\n    mcp-alias-secret: \"" + secret + "\"\n"
	if err := ValidateV8Config([]byte(raw)); err != nil {
		t.Fatalf("v8 validation rejected upstream.claude.mcp-alias-secret: %v", err)
	}
	cfg, err := LoadConfig(writeRoutingConfig(t, path, raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeCode.MCPAliasSecret != secret {
		t.Fatalf("v8 layout: MCPAliasSecret = %q, want %q", cfg.ClaudeCode.MCPAliasSecret, secret)
	}

	// Saves of an unrelated field keep it, in the v8 layout and the legacy one.
	for i, migrate := range []bool{true, false} {
		cfg.Port = 8400 + i
		if err = SaveConfigPreserveComments(path, cfg, migrate); err != nil {
			t.Fatal(err)
		}
		if cfg, err = LoadConfig(path); err != nil {
			t.Fatal(err)
		}
		if cfg.ClaudeCode.MCPAliasSecret != secret {
			saved, _ := os.ReadFile(path)
			t.Fatalf("save (migrate=%v) lost mcp-alias-secret:\n%s", migrate, saved)
		}
	}

	// The legacy layout names it under claude-code.
	legacy := filepath.Join(t.TempDir(), "config.yaml")
	if cfg, err = LoadConfig(writeRoutingConfig(t, legacy, "port: 8317\nclaude-code:\n  mcp-alias-secret: "+secret+"\n")); err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeCode.MCPAliasSecret != secret {
		t.Fatalf("legacy layout: MCPAliasSecret = %q, want %q", cfg.ClaudeCode.MCPAliasSecret, secret)
	}

	view, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(view), secret) {
		t.Fatal("the config's JSON view carries mcp-alias-secret")
	}

	// Unset, it never appears in a saved file.
	fresh, err := LoadConfig(writeRoutingConfig(t, path, "config-version: 8\nserver:\n  port: 8317\n"))
	if err != nil {
		t.Fatal(err)
	}
	fresh.Port = 8500
	if err = SaveConfigPreserveComments(path, fresh, true); err != nil {
		t.Fatal(err)
	}
	if saved, _ := os.ReadFile(path); strings.Contains(string(saved), "mcp-alias-secret") {
		t.Fatalf("a save invented mcp-alias-secret:\n%s", saved)
	}
}
