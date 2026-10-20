package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// devyreCallerContext is a request context whose caller authenticated with key.
func devyreCallerContext(base context.Context, key string) context.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("userApiKey", key)
	return context.WithValue(base, "gin", ginCtx)
}

func TestDevyreClaudeMCPAliasSecretReplacesTheCallersKey(t *testing.T) {
	one := devyreCallerContext(context.Background(), "caller-key-before-a-restart")
	two := devyreCallerContext(context.Background(), "caller-key-after-a-restart")
	cfg := &config.Config{}
	cfg.ClaudeCode.MCPAliasSecret = "  per-install-alias-secret  "
	first, second := resolveClaudeMCPAliasOptions(one, cfg).secret, resolveClaudeMCPAliasOptions(two, cfg).secret
	if first != "per-install-alias-secret" || second != first {
		t.Fatalf("configured secret: callers got %q and %q, want both per-install-alias-secret", first, second)
	}

	// Unset or blank, the caller's key keys the aliases, as upstream does.
	for _, unset := range []*config.Config{nil, {}, {SDKConfig: config.SDKConfig{ClaudeCode: config.ClaudeCodeConfig{MCPAliasSecret: " \t"}}}} {
		if got := resolveClaudeMCPAliasOptions(one, unset).secret; got != "caller-key-before-a-restart" {
			t.Fatalf("no configured secret: alias secret %q, want the caller's key", got)
		}
	}
}

// Through the executor: the names a cloaked request gives the tools upstream,
// in the tool list and in the history's tool calls, for two caller keys.
func TestDevyreClaudeMCPAliasSecretKeepsUpstreamToolNamesAcrossKeys(t *testing.T) {
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)

	var bodies [][]byte
	transport := datePinRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		bodies = append(bodies, body)
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		header.Set("request-id", fmt.Sprintf("req_alias_%d", len(bodies)))
		response := fmt.Sprintf(`{"id":"msg_alias_%d","type":"message","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":10,"output_tokens":2}}`, len(bodies))
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	auth := &cliproxyauth.Auth{
		ID:         "devyre-alias-oauth",
		Attributes: map[string]string{"api_key": "sk-ant-oat-devyre-alias", "cloak_mode": "always"},
		Metadata: map[string]any{
			"account_uuid":                        "11111111-2222-4333-8444-555555555555",
			claudeauth.ClaudeDeviceIDsMetadataKey: []string{"00000000000000000000000000000000000000000000000000000000000000a1"},
		},
	}
	payload := []byte(`{
		"model": "claude-opus-5",
		"max_tokens": 100,
		"tools": [
			{"name": "Bash", "description": "Run a command", "input_schema": {"type": "object"}},
			{"name": "Read", "description": "Read a file", "input_schema": {"type": "object"}}
		],
		"messages": [
			{"role": "user", "content": "List the files."},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command": "ls"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"}]}
		]
	}`)
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Session-ID": []string{"devyre-alias-session"}}}
	names := func(cfg *config.Config, key string) (tools, call string) {
		t.Helper()
		ctx := context.WithValue(devyreCallerContext(context.Background(), key), "cliproxy.roundtripper", http.RoundTripper(transport))
		before := len(bodies)
		if _, err := NewClaudeExecutor(cfg).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}, options); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(bodies) != before+1 {
			t.Fatalf("upstream requests = %d, want %d", len(bodies), before+1)
		}
		body := bodies[len(bodies)-1]
		tools = gjson.GetBytes(body, "tools.#.name").Raw
		call = gjson.GetBytes(body, "messages.1.content.0.name").String()
		if !strings.Contains(tools, "mcp__") || !strings.HasPrefix(call, "mcp__") {
			t.Fatalf("the tools were not aliased: tools %s, call %q", tools, call)
		}
		return tools, call
	}

	configured := &config.Config{}
	configured.ClaudeCode.MCPAliasSecret = "per-install-alias-secret"
	toolsBefore, callBefore := names(configured, "caller-key-before-a-restart")
	toolsAfter, callAfter := names(configured, "caller-key-after-a-restart")
	if toolsBefore != toolsAfter || callBefore != callAfter {
		t.Fatalf("with mcp-alias-secret a new caller key renamed the tools: %s %q -> %s %q", toolsBefore, callBefore, toolsAfter, callAfter)
	}

	// Without it, the new key names every tool anew, which the prompt cache cannot follow.
	plainBefore, _ := names(&config.Config{}, "caller-key-before-a-restart")
	plainAfter, _ := names(&config.Config{}, "caller-key-after-a-restart")
	if plainBefore == plainAfter {
		t.Fatalf("without mcp-alias-secret two caller keys gave the same names %s; the test no longer shows what the setting changes", plainBefore)
	}
}
