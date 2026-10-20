package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// A reload that changes claude-code.mcp-alias-secret says so, and never with its value.
func TestDevyreConfigDiffMCPAliasSecret(t *testing.T) {
	with := func(secret string) *config.Config {
		cfg := &config.Config{}
		cfg.ClaudeCode.MCPAliasSecret = secret
		return cfg
	}
	for _, tc := range []struct {
		old, new, want string
	}{
		{"", "first-alias-secret", "claude-code.mcp-alias-secret: created"},
		{"first-alias-secret", "", "claude-code.mcp-alias-secret: deleted"},
		{"first-alias-secret", "second-alias-secret", "claude-code.mcp-alias-secret: updated"},
	} {
		details := BuildConfigChangeDetails(with(tc.old), with(tc.new))
		expectContains(t, details, tc.want)
		for _, line := range details {
			if (tc.old != "" && strings.Contains(line, tc.old)) || (tc.new != "" && strings.Contains(line, tc.new)) {
				t.Fatalf("the change details name the secret: %q", line)
			}
		}
	}
	for _, same := range [][2]string{{"", ""}, {"alias-secret", " alias-secret "}} {
		for _, line := range BuildConfigChangeDetails(with(same[0]), with(same[1])) {
			if strings.Contains(line, "mcp-alias-secret") {
				t.Fatalf("an unchanged secret was reported: %q", line)
			}
		}
	}
}
