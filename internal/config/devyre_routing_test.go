package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestExpiringFirstRoutingConfigDefaults(t *testing.T) {
	var routing RoutingConfig
	if got := routing.ExpiringFirst.GatePercent(); got != 2 {
		t.Fatalf("default gate = %v, want 2", got)
	}
	cache := routing.QuotaObservation.UsageCache
	if !cache.IsEnabled() || cache.ClaudeUsageTTLDuration() != 5*time.Minute ||
		cache.CodexUsageTTLDuration() != 60*time.Second || cache.RefreshFloorDuration() != 30*time.Second {
		t.Fatalf("usage cache defaults = %v %v %v %v", cache.IsEnabled(), cache.ClaudeUsageTTLDuration(), cache.CodexUsageTTLDuration(), cache.RefreshFloorDuration())
	}
	poller := routing.QuotaObservation.Poller
	if poller.ClaudeIntervalDuration() != 30*time.Minute || poller.ClaudeMinGapDuration() != 10*time.Minute || poller.CodexIntervalDuration() != 5*time.Minute {
		t.Fatalf("poller defaults = %v %v %v", poller.ClaudeIntervalDuration(), poller.ClaudeMinGapDuration(), poller.CodexIntervalDuration())
	}

	gate := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		value *float64
		want  float64
	}{
		{value: gate(0), want: 0},
		{value: gate(7.5), want: 7.5},
		{value: gate(-5), want: 0},
		{value: gate(150), want: 100},
		{value: gate(math.NaN()), want: 2},
		{value: gate(math.Inf(1)), want: 100},
	} {
		if got := (ExpiringFirstConfig{GateRemainingPercent: tc.value}).GatePercent(); got != tc.want {
			t.Fatalf("GatePercent(%v) = %v, want %v", *tc.value, got, tc.want)
		}
	}

	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{raw: "", want: 5 * time.Minute},
		{raw: "90s", want: 90 * time.Second},
		{raw: " 2m30s ", want: 150 * time.Second},
		{raw: "60", want: 5 * time.Minute},
		{raw: "soon", want: 5 * time.Minute},
		{raw: "0s", want: 5 * time.Minute},
		{raw: "-1m", want: 5 * time.Minute},
	} {
		if got := (UsageCacheConfig{ClaudeUsageTTL: tc.raw}).ClaudeUsageTTLDuration(); got != tc.want {
			t.Fatalf("ClaudeUsageTTLDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	off := false
	if (UsageCacheConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("explicit enabled: false must disable the usage cache")
	}
}

func TestExpiringFirstStrategyAliases(t *testing.T) {
	for strategy, want := range map[string]bool{
		"expiring-first":   true,
		" Expiring-First ": true,
		"expiringfirst":    true,
		"EF":               true,
		"soonest-reset":    true,
		"round-robin":      false,
		"fill-first":       false,
		"":                 false,
		"expiring":         false,
	} {
		if got := IsExpiringFirstStrategy(strategy); got != want {
			t.Fatalf("IsExpiringFirstStrategy(%q) = %v, want %v", strategy, got, want)
		}
	}

	on, off := true, false
	for _, tc := range []struct {
		strategy string
		enabled  *bool
		want     bool
	}{
		{strategy: "expiring-first", want: true},
		{strategy: "ef", want: true},
		{strategy: "round-robin", want: false},
		{strategy: "", want: false},
		{strategy: "expiring-first", enabled: &off, want: false},
		{strategy: "fill-first", enabled: &on, want: true},
	} {
		routing := RoutingConfig{Strategy: tc.strategy}
		routing.QuotaObservation.Poller.Enabled = tc.enabled
		if got := routing.QuotaPollerEnabled(); got != tc.want {
			t.Fatalf("QuotaPollerEnabled(strategy=%q, enabled=%v) = %v, want %v", tc.strategy, tc.enabled, got, tc.want)
		}
	}
}

// routingBlocksYAML sets every new key, using explicit zero values where a
// pointer distinguishes them from unset.
const routingBlocksYAML = `  expiring-first:
    gate-remaining-percent: 0
    log-picks: true
  quota-observation:
    usage-cache:
      enabled: false
      claude-usage-ttl: 7m
      codex-usage-ttl: 90s
      refresh-floor: 45s
    poller:
      enabled: false
      claude-interval: 45m
      claude-min-gap: 15m
      codex-interval: 6m
`

func assertRoutingBlocks(t *testing.T, stage string, routing RoutingConfig) {
	t.Helper()
	ef := routing.ExpiringFirst
	if ef.GateRemainingPercent == nil || *ef.GateRemainingPercent != 0 || ef.GatePercent() != 0 || !ef.LogPicks {
		t.Fatalf("%s: expiring-first = %+v", stage, ef)
	}
	cache := routing.QuotaObservation.UsageCache
	if cache.Enabled == nil || cache.IsEnabled() || cache.ClaudeUsageTTLDuration() != 7*time.Minute ||
		cache.CodexUsageTTLDuration() != 90*time.Second || cache.RefreshFloorDuration() != 45*time.Second {
		t.Fatalf("%s: usage-cache = %+v", stage, cache)
	}
	poller := routing.QuotaObservation.Poller
	if poller.Enabled == nil || routing.QuotaPollerEnabled() || poller.ClaudeIntervalDuration() != 45*time.Minute ||
		poller.ClaudeMinGapDuration() != 15*time.Minute || poller.CodexIntervalDuration() != 6*time.Minute {
		t.Fatalf("%s: poller = %+v", stage, poller)
	}
}

// assertSavedV8Routing checks a saved document: valid v8, the blocks live under
// routing, and the migration did not archive any of them as unknown comments.
func assertSavedV8Routing(t *testing.T, stage, path string) {
	t.Helper()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(saved); err != nil {
		t.Fatalf("%s: saved config is not valid v8: %v\n%s", stage, err, saved)
	}
	if !strings.Contains(string(saved), "config-version: 8") {
		t.Fatalf("%s: saved config is not in the v8 layout:\n%s", stage, saved)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"routing.expiring-first.gate-remaining-percent",
		"routing.expiring-first.log-picks",
		"routing.quota-observation.usage-cache.enabled",
		"routing.quota-observation.usage-cache.refresh-floor",
		"routing.quota-observation.poller.enabled",
		"routing.quota-observation.poller.codex-interval",
	} {
		if yamlPath(doc.Content[0], key) == nil {
			t.Fatalf("%s: %s missing from the saved config:\n%s", stage, key, saved)
		}
	}
	if strings.Contains(string(saved), "# routing.") {
		t.Fatalf("%s: a routing setting was archived as an unknown section:\n%s", stage, saved)
	}
}

func TestExpiringFirstRoutingConfigSurvivesV8SaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver:\n  port: 8317\nrouting:\n  strategy: expiring-first\n  session-affinity: true\n" + routingBlocksYAML
	if err := ValidateV8Config([]byte(raw)); err != nil {
		t.Fatalf("v8 validation rejected the routing blocks: %v", err)
	}
	cfg, err := LoadConfig(writeRoutingConfig(t, path, raw))
	if err != nil {
		t.Fatal(err)
	}
	assertRoutingBlocks(t, "load", cfg.Routing)

	// A v8 management save (migrate=true) and a v0 save (migrate=false) of an
	// unrelated field must both keep every routing setting.
	for i, migrate := range []bool{true, false} {
		cfg.Port = 8400 + i
		if err = SaveConfigPreserveComments(path, cfg, migrate); err != nil {
			t.Fatalf("save (migrate=%v): %v", migrate, err)
		}
		assertSavedV8Routing(t, "save", path)
		reloaded, errLoad := LoadConfig(path)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if reloaded.Port != 8400+i {
			t.Fatalf("save (migrate=%v) lost the port change", migrate)
		}
		assertRoutingBlocks(t, "reload", reloaded.Routing)
		cfg = reloaded
	}
}

func TestExpiringFirstRoutingConfigSurvivesLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "port: 8317\nrequest-retry: 2\nrouting:\n  strategy: ef\n" + routingBlocksYAML
	cfg, err := LoadConfig(writeRoutingConfig(t, path, raw))
	if err != nil {
		t.Fatal(err)
	}
	if saved, _ := os.ReadFile(path); string(saved) != raw {
		t.Fatal("loading a legacy config rewrote it")
	}
	assertRoutingBlocks(t, "legacy load", cfg.Routing)
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	assertSavedV8Routing(t, "migration", path)
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.RequestRetry != 2 || reloaded.Routing.Strategy != "ef" {
		t.Fatalf("migration changed unrelated settings: retry=%d strategy=%q", reloaded.RequestRetry, reloaded.Routing.Strategy)
	}
	assertRoutingBlocks(t, "migrated reload", reloaded.Routing)
}

// TestExpiringFirstRoutingConfigSurvivesManagementWrite replays the
// /v8/management/config write pipeline (ConfigV8): normalize the stored file,
// patch the YAML tree, validate, normalize again, write, reload. A later v0
// style save must keep what the v8 write stored.
func TestExpiringFirstRoutingConfigSurvivesManagementWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# Operator comment\nconfig-version: 8\nserver:\n  port: 8317\nrouting:\n  strategy: round-robin\n"
	writeRoutingConfig(t, path, raw)

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err = NormalizeConfigLayout(stored, true)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(stored, &doc); err != nil {
		t.Fatal(err)
	}
	var patch yaml.Node
	if err = yaml.Unmarshal([]byte("routing:\n  strategy: expiring-first\n"+routingBlocksYAML), &patch); err != nil {
		t.Fatal(err)
	}
	if err = NormalizeV8ConfigAliases(patch.Content[0]); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"routing.strategy", "routing.expiring-first", "routing.quota-observation"} {
		setYAMLPath(doc.Content[0], key, yamlPath(patch.Content[0], key))
	}
	if err = NormalizeV8ConfigAliases(doc.Content[0]); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseConfigBytes(data); err != nil {
		t.Fatalf("management write rejected by ParseConfigBytes: %v", err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("management write rejected by ValidateV8Config: %v", err)
	}
	if data, _, err = NormalizeConfigLayout(data, true); err != nil {
		t.Fatal(err)
	}
	writeRoutingConfig(t, path, string(NormalizeCommentIndentation(data)))
	assertSavedV8Routing(t, "management write", path)
	if saved, _ := os.ReadFile(path); !strings.Contains(string(saved), "# Operator comment") {
		t.Fatal("management write dropped the operator comment")
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsExpiringFirstStrategy(cfg.Routing.Strategy) {
		t.Fatalf("strategy = %q", cfg.Routing.Strategy)
	}
	assertRoutingBlocks(t, "management reload", cfg.Routing)

	cfg.Routing.Strategy = "fill-first"
	if err = SaveConfigPreserveComments(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	assertSavedV8Routing(t, "persist after management write", path)
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Routing.Strategy != "fill-first" {
		t.Fatalf("strategy = %q, want fill-first", reloaded.Routing.Strategy)
	}
	assertRoutingBlocks(t, "persist reload", reloaded.Routing)
}

// A save that introduces the blocks from memory must keep explicit zero
// values, and a save that never set them must not add them.
func TestExpiringFirstRoutingConfigSaveAddsExplicitValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver:\n  port: 8317\nrouting:\n  strategy: round-robin\n"
	cfg, err := LoadConfig(writeRoutingConfig(t, path, raw))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = 8318
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	if saved, _ := os.ReadFile(path); strings.Contains(string(saved), "expiring-first") || strings.Contains(string(saved), "quota-observation") {
		t.Fatalf("a save without routing changes added the new blocks:\n%s", saved)
	}

	var blocks RoutingConfig
	if err = yaml.Unmarshal([]byte(routingBlocksYAML), &blocks); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.ExpiringFirst = blocks.ExpiringFirst
	cfg.Routing.QuotaObservation = blocks.QuotaObservation
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	assertSavedV8Routing(t, "save from memory", path)
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	assertRoutingBlocks(t, "reload from memory", reloaded.Routing)

	// Only the explicitly set pointer keeps an otherwise empty section.
	off := false
	fresh, err := LoadConfig(writeRoutingConfig(t, path, raw))
	if err != nil {
		t.Fatal(err)
	}
	fresh.Routing.QuotaObservation.Poller.Enabled = &off
	if err = SaveConfigPreserveComments(path, fresh, true); err != nil {
		t.Fatal(err)
	}
	reloaded, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Routing.QuotaObservation.Poller.Enabled == nil || *reloaded.Routing.QuotaObservation.Poller.Enabled {
		t.Fatal("explicit poller.enabled: false was dropped by the save")
	}
	if reloaded.Routing.ExpiringFirst.GateRemainingPercent != nil || reloaded.Routing.QuotaObservation.UsageCache.Enabled != nil {
		t.Fatal("the save invented settings that were never set")
	}
}

func TestExpiringFirstRoutingConfigUnknownKeysStayStrict(t *testing.T) {
	for _, raw := range []string{
		"config-version: 8\nrouting:\n  expiring-first:\n    gate-percent: 5\n",
		"config-version: 8\nrouting:\n  quota-observation:\n    poller:\n      interval: 5m\n",
	} {
		if err := ValidateV8Config([]byte(raw)); err == nil {
			t.Fatalf("ValidateV8Config accepted a misspelled key:\n%s", raw)
		}
	}
	raw := "port: 8317\nrouting:\n  quota-observation:\n    poller:\n      enabled: false\n      interval: 5m\n"
	migrated, _, err := NormalizeConfigLayout([]byte(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migrated), "# routing.quota-observation.poller.interval: 5m") {
		t.Fatalf("migration did not archive the unknown nested key:\n%s", migrated)
	}
	cfg, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if enabled := cfg.Routing.QuotaObservation.Poller.Enabled; enabled == nil || *enabled {
		t.Fatalf("migration lost the known poller.enabled setting:\n%s", migrated)
	}
}

func TestExpiringFirstRoutingConfigExampleIsValid(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(example), "\r\n", "\n")
	if !strings.Contains(text, "expiring-first") {
		t.Fatal("config.example.yaml does not document the expiring-first strategy")
	}
	lines := strings.Split(text, "\n")
	start, end := -1, -1
	for i, line := range lines {
		if line == "  # expiring-first:" {
			start = i
		}
		if start >= 0 && strings.Contains(line, "codex-interval:") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatal("config.example.yaml is missing the commented expiring-first and quota-observation blocks")
	}
	// Uncomment the two blocks the way an operator would; prose between them stays a comment.
	for i := start; i <= end; i++ {
		if lines[i] == "" {
			continue
		}
		rest, ok := strings.CutPrefix(lines[i], "  # ")
		if !ok {
			t.Fatalf("example line %d is not a commented routing line: %q", i+1, lines[i])
		}
		if strings.HasPrefix(rest, " ") || rest == "expiring-first:" || rest == "quota-observation:" {
			lines[i] = "  " + rest
		}
	}
	uncommented := []byte(strings.Join(lines, "\n"))
	if err = ValidateV8Config(uncommented); err != nil {
		t.Fatalf("uncommented routing example is invalid: %v", err)
	}
	cfg, err := ParseConfigBytes(uncommented)
	if err != nil {
		t.Fatal(err)
	}
	routing := cfg.Routing
	cache, poller := routing.QuotaObservation.UsageCache, routing.QuotaObservation.Poller
	if routing.ExpiringFirst.GateRemainingPercent == nil || routing.ExpiringFirst.GatePercent() != 2 ||
		cache.Enabled == nil || !cache.IsEnabled() || cache.ClaudeUsageTTL != "5m" || cache.CodexUsageTTL != "60s" || cache.RefreshFloor != "30s" ||
		poller.Enabled == nil || poller.ClaudeInterval != "30m" || poller.ClaudeMinGap != "10m" || poller.CodexInterval != "5m" {
		t.Fatalf("uncommented example does not show the defaults: %+v", routing)
	}
}

// writeRoutingConfig writes raw to path and returns path.
func writeRoutingConfig(t *testing.T, path, raw string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
