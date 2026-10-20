package session

import (
	"net/http"
	"testing"
)

// Claude Code's own requests, as CLI 2.1.304 sends them to a gateway with
// CLAUDE_CODE_GATEWAY_HINT_HEADERS=1: a subagent names its agent type, "fork"
// for a fork; without the variable a subagent sends its agent id alone.
func TestExtractSessionInfoClaudeAgentType(t *testing.T) {
	t.Parallel()

	request := func(agentID, agentType string) http.Header {
		headers := http.Header{"X-Claude-Code-Session-Id": []string{"root-123"}}
		if agentID != "" {
			headers.Set("X-Claude-Code-Agent-Id", agentID)
		}
		if agentType != "" {
			headers.Set("X-Claude-Code-Agent-Type", agentType)
		}
		return headers
	}
	for _, tc := range []struct {
		name      string
		headers   http.Header
		session   string
		parent    string
		agentType string
		fork      bool
	}{
		{name: "fork", headers: request("a1", "fork"), session: "claude:root-123:agent:a1", parent: "claude:root-123", agentType: "fork", fork: true},
		{name: "general-purpose subagent", headers: request("a2", "general-purpose"), session: "claude:root-123:agent:a2", parent: "claude:root-123", agentType: "general-purpose"},
		{name: "built-in type in capitals", headers: request("a3", "Explore"), session: "claude:root-123:agent:a3", parent: "claude:root-123", agentType: "explore"},
		{name: "subagent that does not say", headers: request("a4", ""), session: "claude:root-123:agent:a4", parent: "claude:root-123"},
		{name: "a value that is no agent type", headers: request("a5", "fork; drop"), session: "claude:root-123:agent:a5", parent: "claude:root-123"},
		{name: "main thread naming a type", headers: request("", "fork"), session: "claude:root-123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, ok := ExtractSessionInfo(tc.headers, nil, nil)
			if !ok {
				t.Fatal("ExtractSessionInfo() found no session")
			}
			if info.SessionID != tc.session || info.ParentSessionID != tc.parent || info.AgentType != tc.agentType || info.IsFork != tc.fork {
				t.Fatalf("ExtractSessionInfo() = session %q parent %q agentType %q fork %v; want %q %q %q %v",
					info.SessionID, info.ParentSessionID, info.AgentType, info.IsFork, tc.session, tc.parent, tc.agentType, tc.fork)
			}
		})
	}
}

// A request whose session comes from metadata.user_id names its agent type the
// same way.
func TestExtractSessionInfoClaudeMetadataAgentType(t *testing.T) {
	t.Parallel()

	headers := http.Header{
		"X-Claude-Code-Agent-Id":   []string{"a1"},
		"X-Claude-Code-Agent-Type": []string{"fork"},
	}
	payload := []byte(`{"metadata": {"user_id": "{\"device_id\":\"dev-1\",\"session_id\":\"main-456\"}"}}`)
	info, ok := ExtractSessionInfo(headers, payload, nil)
	if !ok {
		t.Fatal("ExtractSessionInfo() found no session")
	}
	if info.SessionID != "claude:main-456:agent:a1" || info.ParentSessionID != "claude:main-456" || info.AgentType != "fork" || !info.IsFork {
		t.Fatalf("ExtractSessionInfo() = session %q parent %q agentType %q fork %v", info.SessionID, info.ParentSessionID, info.AgentType, info.IsFork)
	}
}
