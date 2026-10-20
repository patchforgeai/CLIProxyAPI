package session

import (
	"net/http"
	"regexp"
	"strings"
)

// devyre: Claude Code names the kind of agent a request comes from when it
// talks to a gateway (CLAUDE_CODE_GATEWAY_HINT_HEADERS=1, which Anvil sets for
// its routed sessions): X-Claude-Code-Agent-Type is a built-in subagent's type
// (fork, general-purpose, Explore, Plan, ...), "custom" for one the user
// defined, "teammate" for a teammate, and absent on the main thread. Checked
// against CLI 2.1.296 and 2.1.304. Without it, a fork's headers are those of
// any subagent (X-Claude-Code-Agent-Id alone), although a fork carries its
// parent's conversation and reads the parent's prompt cache, while another
// subagent starts a fresh context. The two Claude branches of
// ExtractSessionInfo call applyClaudeAgentType.

// ClaudeAgentTypeHeader is the header Claude Code names a request's agent type in.
const ClaudeAgentTypeHeader = "X-Claude-Code-Agent-Type"

// ClaudeForkAgentType is the agent type of a Claude Code fork.
const ClaudeForkAgentType = "fork"

// claudeAgentTypePattern is what an agent type may be: Claude Code's built-in
// names and the two kinds it sends for other agents.
var claudeAgentTypePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// applyClaudeAgentType records the agent type a Claude Code subagent's request
// names, lowercased, and marks a fork as one. A value that is not an agent
// type is ignored, as if the client had said nothing.
func applyClaudeAgentType(info *SessionInfo, headers http.Header) {
	agentType := claudeAgentType(headers)
	if agentType == "" {
		return
	}
	info.AgentType = agentType
	if agentType == ClaudeForkAgentType {
		info.IsFork = true
	}
}

// claudeAgentType returns the request's agent type, or "" when it names none.
func claudeAgentType(headers http.Header) string {
	for key, values := range headers {
		if !strings.EqualFold(key, ClaudeAgentTypeHeader) {
			continue
		}
		for _, value := range values {
			if agentType := strings.ToLower(strings.TrimSpace(value)); claudeAgentTypePattern.MatchString(agentType) {
				return agentType
			}
		}
	}
	return ""
}
