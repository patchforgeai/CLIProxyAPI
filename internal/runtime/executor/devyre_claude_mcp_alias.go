package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// devyre: the secret behind a cloaked request's tool names, when the config
// sets one.
//
// A cloaked Claude request names each of the caller's tools as a tool of a
// virtual MCP server (mcp__<two words>__<word>_<Name>), keyed by the caller's
// API key so that every caller has names of its own. A caller whose key changes
// gets new names for every tool, and the tools lead every request's prompt:
// each of its conversations then misses the prompt cache at its next request
// and is written to it whole again. Anvil makes new client keys at every start
// of its server, so every start rewrote the context of every session that went
// on. claude-code.mcp-alias-secret (upstream.claude.mcp-alias-secret in the v8
// layout) keys the names in place of the caller's key: a secret made once per
// installation keeps the names, and the caches, whatever keys the callers use.

// devyreClaudeMCPAliasSecret returns the configured alias secret, or "" to key
// the aliases by the caller's API key, as upstream does.
func devyreClaudeMCPAliasSecret(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ClaudeCode.MCPAliasSecret)
}
