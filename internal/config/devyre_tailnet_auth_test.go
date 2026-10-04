package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Fake values only: a CGNAT tailnet range, an example.com login and an example
// tailnet name.
const devyreTailnetV8Block = `management:
  allow-remote: true
  # Filled by devyre/scripts/tailnet-trust.ps1.
  tailnet-auth:
    enabled: true
    allowed-logins:
      - "owner@example.com"
    allowed-devices:
      - "100.64.0.10"
      - "fd7a:115c:a1e0::10"
    allowed-hosts:
      - "devbox"
      - "devbox.example-tailnet.ts.net"
      - "localhost"
      - "127.0.0.1"
    allow-local: true # local software is trusted
    proxy-api: false
`

func devyreWantTailnetAuth() TailnetAuthConfig {
	return TailnetAuthConfig{
		Enabled:        true,
		AllowedLogins:  []string{"owner@example.com"},
		AllowedDevices: []string{"100.64.0.10", "fd7a:115c:a1e0::10"},
		AllowedHosts:   []string{"devbox", "devbox.example-tailnet.ts.net", "localhost", "127.0.0.1"},
		AllowLocal:     true,
		ProxyAPI:       false,
	}
}

func TestTailnetAuthConfig_DefaultsAreOff(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nserver:\n  port: 8317\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.RemoteManagement.TailnetAuth
	if got.Enabled || got.AllowLocal || got.ProxyAPI || len(got.AllowedLogins)+len(got.AllowedDevices)+len(got.AllowedHosts) != 0 {
		t.Fatalf("defaults = %+v, want everything off and every list empty", got)
	}
}

func TestTailnetAuthConfig_LoadsFromBothLayouts(t *testing.T) {
	v8, err := ParseConfigBytes([]byte("config-version: 8\n" + devyreTailnetV8Block))
	if err != nil {
		t.Fatal(err)
	}
	if got := v8.RemoteManagement.TailnetAuth; !reflect.DeepEqual(got, devyreWantTailnetAuth()) {
		t.Fatalf("v8 layout = %+v", got)
	}

	legacy := strings.Replace(devyreTailnetV8Block, "management:", "remote-management:", 1)
	old, err := ParseConfigBytes([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if got := old.RemoteManagement.TailnetAuth; !reflect.DeepEqual(got, devyreWantTailnetAuth()) {
		t.Fatalf("legacy layout = %+v", got)
	}
	migrated, changed, err := NormalizeConfigLayout([]byte(legacy), true)
	if err != nil || !changed {
		t.Fatalf("migrate legacy layout: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(migrated), "management:\n") || strings.Contains(string(migrated), "remote-management") {
		t.Fatalf("migration did not move tailnet-auth under management:\n%s", migrated)
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatalf("migrated config is not valid v8: %v\n%s", errValidate, migrated)
	}
	again, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.RemoteManagement.TailnetAuth; !reflect.DeepEqual(got, devyreWantTailnetAuth()) {
		t.Fatalf("migrated tailnet-auth = %+v", got)
	}
}

func TestTailnetAuthConfig_V8ValidationRejectsUnknownKeys(t *testing.T) {
	valid := "config-version: 8\n" + devyreTailnetV8Block
	if err := ValidateV8Config([]byte(valid)); err != nil {
		t.Fatalf("valid block rejected: %v", err)
	}
	typo := strings.Replace(valid, "allowed-devices:", "allowed-device:", 1)
	if err := ValidateV8Config([]byte(typo)); err == nil {
		t.Fatal("a misspelled tailnet-auth key was accepted")
	}
}

// A legacy (v0) save rewrites the whole config from memory. The block must keep
// its values and comments, including explicit false and empty lists, and a
// config without the block must not gain one.
func TestTailnetAuthConfig_SavePreservesTheBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := "# Operator comment\nconfig-version: 8\nserver:\n  port: 8317\n" + devyreTailnetV8Block
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Debug = true // an unrelated v0 edit
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, want := range []string{"# Operator comment", "# Filled by devyre/scripts/tailnet-trust.ps1.", "# local software is trusted", "proxy-api: false", "management:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("save lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "remote-management") {
		t.Fatalf("save reintroduced the legacy layout:\n%s", text)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.RemoteManagement.TailnetAuth; !reflect.DeepEqual(got, devyreWantTailnetAuth()) || !reloaded.Debug {
		t.Fatalf("reloaded tailnet-auth = %+v debug=%v", got, reloaded.Debug)
	}

	// Explicitly disabled with empty lists: still explicit after a save.
	off := "config-version: 8\nserver:\n  port: 8317\nmanagement:\n  tailnet-auth:\n    enabled: false\n    allowed-logins: []\n    allowed-devices: []\n    allowed-hosts: []\n    allow-local: false\n    proxy-api: false\n"
	if err = os.WriteFile(path, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg.Debug = true
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, _ = os.ReadFile(path)
	for _, want := range []string{"enabled: false", "allowed-logins: []", "allowed-devices: []", "allowed-hosts: []", "allow-local: false", "proxy-api: false"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("save dropped explicit %q:\n%s", want, saved)
		}
	}

	// No block: a save does not add one.
	plain := "config-version: 8\nserver:\n  port: 8317\nmanagement:\n  allow-remote: true\n"
	if err = os.WriteFile(path, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg.Debug = true
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	if saved, _ = os.ReadFile(path); strings.Contains(string(saved), "tailnet-auth") {
		t.Fatalf("save added a default tailnet-auth block:\n%s", saved)
	}
}

func TestTailnetAuthConfig_V8PathsAreKnown(t *testing.T) {
	want := map[string]string{
		"remote-management.tailnet-auth.enabled":         "management.tailnet-auth.enabled",
		"remote-management.tailnet-auth.allowed-logins":  "management.tailnet-auth.allowed-logins",
		"remote-management.tailnet-auth.allowed-devices": "management.tailnet-auth.allowed-devices",
		"remote-management.tailnet-auth.allowed-hosts":   "management.tailnet-auth.allowed-hosts",
		"remote-management.tailnet-auth.allow-local":     "management.tailnet-auth.allow-local",
		"remote-management.tailnet-auth.proxy-api":       "management.tailnet-auth.proxy-api",
	}
	for _, path := range v8Paths {
		if current, ok := want[path.old]; ok {
			if path.current != current {
				t.Fatalf("%s maps to %s, want %s", path.old, path.current, current)
			}
			delete(want, path.old)
		}
	}
	if len(want) != 0 {
		t.Fatalf("v8 paths missing: %v", want)
	}
}

// config.example.yaml documents the block commented out; uncommented, it is a
// valid v8 block whose values equal the code defaults (everything off).
func TestTailnetAuthConfig_ExampleIsValidAndOff(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.ReplaceAll(string(example), "\r\n", "\n"), "\n")
	start, end := -1, -1
	for i, line := range lines {
		if line == "  # tailnet-auth:" {
			start = i
		}
		if start >= 0 && strings.HasPrefix(line, "  #   proxy-api:") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatal("config.example.yaml is missing the commented management.tailnet-auth block")
	}
	keys := map[string]bool{}
	for i := start; i <= end; i++ {
		rest, ok := strings.CutPrefix(lines[i], "  # ")
		if !ok {
			t.Fatalf("example line %d is not a commented management line: %q", i+1, lines[i])
		}
		lines[i] = "  " + rest
		if key, _, found := strings.Cut(strings.TrimSpace(rest), ":"); found && i > start {
			keys[key] = true
		}
	}
	for _, key := range []string{"enabled", "allowed-logins", "allowed-devices", "allowed-hosts", "allow-local", "proxy-api"} {
		if !keys[key] {
			t.Fatalf("example block does not document %s", key)
		}
	}
	uncommented := []byte(strings.Join(lines, "\n"))
	if err = ValidateV8Config(uncommented); err != nil {
		t.Fatalf("uncommented tailnet-auth example is invalid: %v", err)
	}
	cfg, err := ParseConfigBytes(uncommented)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RemoteManagement.TailnetAuth; !reflect.DeepEqual(got, TailnetAuthConfig{AllowedLogins: []string{}, AllowedDevices: []string{}, AllowedHosts: []string{}}) {
		t.Fatalf("uncommented example = %+v, want the defaults", got)
	}
}

// The deploy template (devyre/deploy/config.template.yaml) turns tailnet-auth on
// with empty lists that devyre/scripts/tailnet-trust.ps1 fills. This is the typed
// check; TestDevyreDeployTemplate_TailnetAuthBlock pins the raw YAML.
func TestTailnetAuthConfig_DeployTemplateBlock(t *testing.T) {
	raw := readDevyreDeployTemplate(t)
	var doc struct {
		Management struct {
			TailnetAuth map[string]any `yaml:"tailnet-auth"`
		} `yaml:"management"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode deploy template: %v", err)
	}
	if doc.Management.TailnetAuth == nil {
		t.Fatal("deploy template has no management.tailnet-auth block")
	}
	// The loader ignores misspelled keys, so pin the key set on the raw YAML.
	known := map[string]bool{}
	settings := reflect.TypeOf(TailnetAuthConfig{})
	for i := 0; i < settings.NumField(); i++ {
		known[strings.Split(settings.Field(i).Tag.Get("yaml"), ",")[0]] = true
	}
	for key := range doc.Management.TailnetAuth {
		if !known[key] {
			t.Errorf("deploy template tailnet-auth has unknown key %q", key)
		}
		delete(known, key)
	}
	if len(known) != 0 {
		t.Errorf("deploy template tailnet-auth does not set %v; spell every setting out", known)
	}

	rendered := string(raw)
	for placeholder, value := range devyreTemplateValues {
		rendered = strings.ReplaceAll(rendered, placeholder, value)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(rendered template) error = %v", err)
	}
	got := cfg.RemoteManagement.TailnetAuth
	if !got.Enabled || !got.AllowLocal || !got.ProxyAPI {
		t.Errorf("deploy template tailnet-auth = %+v, want enabled, allow-local and proxy-api true", got)
	}
	if len(got.AllowedLogins)+len(got.AllowedDevices)+len(got.AllowedHosts) != 0 {
		t.Errorf("deploy template tailnet-auth lists = %+v, want them empty (tailnet-trust.ps1 fills them at runtime)", got)
	}
}
