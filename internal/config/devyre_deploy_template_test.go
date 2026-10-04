package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// devyre/scripts/new-secrets.ps1 renders devyre/deploy/config.template.yaml into the
// runtime config.yaml. Loading the template here catches misspelled keys, which YAML
// loading would otherwise ignore silently, and pins the placeholders the script fills.

const devyreDeployTemplatePath = "../../devyre/deploy/config.template.yaml"

// devyreTemplateValues mirrors the placeholders new-secrets.ps1 replaces. The values are
// obviously fake and avoid the generated key prefixes.
var devyreTemplateValues = map[string]string{
	"__MANAGEMENT_KEY__":      "template-test-management-key",
	"__KEY_T3_CODE__":         "template-test-key-t3-code",
	"__KEY_CLAUDE_CODE_CLI__": "template-test-key-claude-code-cli",
	"__KEY_CODEX_CLI__":       "template-test-key-codex-cli",
	"__KEY_OTHER_DEVICES__":   "template-test-key-other-devices",
}

func readDevyreDeployTemplate(t *testing.T) []byte {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.FromSlash(devyreDeployTemplatePath))
	if errRead != nil {
		t.Fatalf("read deploy template: %v", errRead)
	}
	return raw
}

func TestDevyreDeployTemplate_PlaceholdersMatchNewSecrets(t *testing.T) {
	script, errRead := os.ReadFile(filepath.FromSlash("../../devyre/scripts/new-secrets.ps1"))
	if errRead != nil {
		t.Fatalf("read new-secrets.ps1: %v", errRead)
	}
	want := make([]string, 0, len(devyreTemplateValues))
	for placeholder := range devyreTemplateValues {
		want = append(want, placeholder)
	}
	sort.Strings(want)

	template := devyreUniqueMatches(regexp.MustCompile(`__[A-Z0-9_]+__`), string(readDevyreDeployTemplate(t)))
	if !reflect.DeepEqual(template, want) {
		t.Errorf("template placeholders = %v, want %v", template, want)
	}
	// new-secrets.ps1 maps each quoted placeholder to a generated secret.
	filled := devyreUniqueMatches(regexp.MustCompile(`'(__[A-Z0-9_]+__)'`), string(script))
	if !reflect.DeepEqual(filled, want) {
		t.Errorf("new-secrets.ps1 fills %v, want %v", filled, want)
	}
}

// devyreUniqueMatches returns the sorted unique matches of re (its first group when present).
func devyreUniqueMatches(re *regexp.Regexp, text string) []string {
	unique := make(map[string]struct{})
	for _, match := range re.FindAllStringSubmatch(text, -1) {
		unique[match[len(match)-1]] = struct{}{}
	}
	out := make([]string, 0, len(unique))
	for value := range unique {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func TestDevyreDeployTemplate_LoadsIntoExpectedConfig(t *testing.T) {
	rendered := string(readDevyreDeployTemplate(t))
	for placeholder, value := range devyreTemplateValues {
		rendered = strings.ReplaceAll(rendered, placeholder, value)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(rendered), 0o600); errWrite != nil {
		t.Fatalf("write rendered config: %v", errWrite)
	}
	cfg, errLoad := LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig(rendered template) error = %v", errLoad)
	}

	if cfg.Host != "" || cfg.Port != 8317 {
		t.Errorf("server host/port = %q/%d, want \"\"/8317", cfg.Host, cfg.Port)
	}
	if want := []string{"127.0.0.1", "172.16.0.0/12", "192.168.65.0/24"}; !reflect.DeepEqual(cfg.TrustedProxies, want) {
		t.Errorf("server.trusted-proxies = %v, want %v", cfg.TrustedProxies, want)
	}

	management := cfg.RemoteManagement
	if !management.AllowRemote {
		t.Error("management.allow-remote = false; the panel and the T3 hub reach the container as remote clients")
	}
	if !looksLikeBcrypt(management.SecretKey) {
		t.Error("management.secret-key was not hashed on load")
	}
	if management.DisableControlPanel || management.DisableAutoUpdatePanel {
		t.Errorf("control panel disabled=%v auto-update disabled=%v, want both false", management.DisableControlPanel, management.DisableAutoUpdatePanel)
	}
	if management.PanelGitHubRepository != "https://github.com/Devyre/Cli-Proxy-API-Management-Center" {
		t.Errorf("management.panel-github-repository = %q, want the Devyre panel fork", management.PanelGitHubRepository)
	}

	wantKeys := []string{
		devyreTemplateValues["__KEY_T3_CODE__"],
		devyreTemplateValues["__KEY_CLAUDE_CODE_CLI__"],
		devyreTemplateValues["__KEY_CODEX_CLI__"],
		devyreTemplateValues["__KEY_OTHER_DEVICES__"],
	}
	if !reflect.DeepEqual(cfg.APIKeys, wantKeys) {
		t.Errorf("access.api-keys = %v, want %v", cfg.APIKeys, wantKeys)
	}

	routing := cfg.Routing
	if routing.Strategy != "expiring-first" {
		t.Errorf("routing.strategy = %q, want expiring-first", routing.Strategy)
	}
	if !routing.SessionAffinity || routing.SessionAffinityTTL != "1h" || routing.SessionAffinitySubagents == nil || !*routing.SessionAffinitySubagents {
		t.Errorf("session affinity = %v ttl %q subagents %v, want true, 1h, true", routing.SessionAffinity, routing.SessionAffinityTTL, routing.SessionAffinitySubagents)
	}
	if cfg.RequestRetry != 3 || cfg.MaxRetryCredentials != 0 || cfg.MaxRetryInterval != 30 {
		t.Errorf("routing.retry = %d/%d/%d, want 3/0/30", cfg.RequestRetry, cfg.MaxRetryCredentials, cfg.MaxRetryInterval)
	}

	if cfg.AuthDir != "/data/auths" {
		t.Errorf("oauth.auth-dir = %q, want /data/auths", cfg.AuthDir)
	}
	if cfg.Debug || !cfg.LoggingToFile || cfg.LogsMaxTotalSizeMB != 200 || cfg.RequestLog {
		t.Errorf("logs debug=%v to-file=%v max-mb=%d request-log=%v, want false/true/200/false", cfg.Debug, cfg.LoggingToFile, cfg.LogsMaxTotalSizeMB, cfg.RequestLog)
	}
	if !cfg.UsageStatisticsEnabled || cfg.RedisUsageQueueRetentionSeconds != 60 {
		t.Errorf("usage statistics=%v retention=%d, want true/60", cfg.UsageStatisticsEnabled, cfg.RedisUsageQueueRetentionSeconds)
	}
	// The loader cleans the plugin dir for the host OS; inside the container it stays /data/plugins.
	if wantDir := filepath.Clean("/data/plugins"); !cfg.Plugins.Enabled || cfg.Plugins.Dir != wantDir {
		t.Errorf("plugins enabled=%v dir=%q, want true and %q", cfg.Plugins.Enabled, cfg.Plugins.Dir, wantDir)
	}
}

// TestDevyreDeployTemplate_RoutingBlocksMatchPlan pins the key names of the expiring-first
// and quota-observation blocks (devyre/PLAN.md, RT-5 and Appendix A) on the raw YAML, so the
// template and the typed routing config cannot drift apart unnoticed.
func TestDevyreDeployTemplate_RoutingBlocksMatchPlan(t *testing.T) {
	var doc struct {
		Routing struct {
			ExpiringFirst    map[string]any `yaml:"expiring-first"`
			QuotaObservation struct {
				UsageCache map[string]any `yaml:"usage-cache"`
				Poller     map[string]any `yaml:"poller"`
			} `yaml:"quota-observation"`
		} `yaml:"routing"`
	}
	if errDecode := yaml.Unmarshal(readDevyreDeployTemplate(t), &doc); errDecode != nil {
		t.Fatalf("decode deploy template: %v", errDecode)
	}

	blocks := []struct {
		name string
		got  map[string]any
		want map[string]any
	}{
		{"routing.expiring-first", doc.Routing.ExpiringFirst, map[string]any{"gate-remaining-percent": 2, "log-picks": true}},
		{"routing.quota-observation.usage-cache", doc.Routing.QuotaObservation.UsageCache, map[string]any{
			"enabled": true, "claude-usage-ttl": "5m", "codex-usage-ttl": "60s", "refresh-floor": "30s",
		}},
		{"routing.quota-observation.poller", doc.Routing.QuotaObservation.Poller, map[string]any{
			"enabled": true, "claude-interval": "30m", "claude-min-gap": "10m", "codex-interval": "5m",
		}},
	}
	for _, block := range blocks {
		if !reflect.DeepEqual(block.got, block.want) {
			t.Errorf("%s = %v, want %v", block.name, block.got, block.want)
		}
		for key, value := range block.got {
			text, isString := value.(string)
			if !isString {
				continue
			}
			if _, errParse := time.ParseDuration(text); errParse != nil {
				t.Errorf("%s.%s = %q is not a Go duration: %v", block.name, key, text, errParse)
			}
		}
	}
}
