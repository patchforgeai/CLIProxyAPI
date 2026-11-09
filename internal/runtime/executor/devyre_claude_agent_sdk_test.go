package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Captured from the real 2.1.321 CLI through SDK 0.3.296, with synthetic
// identities and prompt/tools. Exercise the complete executor, including the
// first-party header path, so recognition alone cannot hide a later rewrite.
func TestDevyreClaudeAgentSDKForwarding(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, operation := range []string{"message", "stream", "count"} {
			for _, subagent := range []bool{false, true} {
				t.Run(fmt.Sprintf("oauth=%v/%s/subagent=%v", oauth, operation, subagent), func(t *testing.T) {
					const userAgent = "claude-cli/2.1.321 (external, sdk-ts, agent-sdk/0.3.296, client-app/anvil/0.0.0-probe)"
					const billing = "x-anthropic-billing-header: cc_version=2.1.321.20d; cc_entrypoint=sdk-ts;"
					const betas = "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,thinking-token-count-2026-05-13,advanced-tool-use-2025-11-20,effort-2025-11-24,thinking-display-updates-2026-08-18,future-client-feature-2099-01-01"
					headers := claudeNativeHelperHeaders(betas, "gzip, deflate, br, zstd")
					headers.Set("User-Agent", userAgent)
					headers.Set("X-Client-App", "anvil/0.0.0-probe")
					headers.Set("X-Stainless-Package-Version", "0.128.0")
					headers.Set("X-Stainless-Runtime-Version", "v26.12.1")
					headers.Set("X-Stainless-Os", "Windows")
					headers.Set("X-Stainless-Arch", "x64")
					payload := []byte(`{"model":"claude-fable-5-2","max_tokens":16000,"system":[{"type":"text","text":` + fmt.Sprintf("%q", billing) + `},{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"Say ok.","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{}}}],"metadata":{"user_id":` + fmt.Sprintf("%q", claudeNativeHelperUserID) + `},"thinking":{"type":"adaptive","display":"updates"},"fallbacks":[{"model":"claude-opus-5-5"}]}`)
					if subagent {
						headers.Set("X-Claude-Code-Agent-Id", "agent-test")
					} else {
						payload, _ = sjson.SetBytes(payload, "system.1.cache_control.ttl", "1h")
						payload, _ = sjson.SetBytes(payload, "messages.0.content.0.cache_control.ttl", "1h")
						headers.Set("Anthropic-Beta", betas+",extended-cache-ttl-2025-04-11")
					}
					if operation == "count" {
						for _, field := range []string{"metadata", "system", "max_tokens", "thinking", "fallbacks"} {
							payload, _ = sjson.DeleteBytes(payload, field)
						}
						headers.Set("Anthropic-Beta", "claude-code-20250219,token-counting-2024-11-01,future-client-feature-2099-01-01")
					}
					var seenBody []byte
					var seenHeaders http.Header
					calls := 0
					transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						seenBody, _ = io.ReadAll(req.Body)
						seenHeaders = req.Header.Clone()
						body := `{"id":"msg_test","type":"message","model":"claude-fable-5-2","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
						contentType := "application/json"
						if operation == "count" {
							body = `{"input_tokens":42}`
						}
						if operation == "stream" {
							contentType = "text/event-stream"
							body = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
					})
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
					auth := &cliproxyauth.Auth{ID: "sdk-test", Attributes: map[string]string{"api_key": "test-api-key"}}
					if oauth {
						auth = claudeNativeHelperOAuthAuth("https://api.anthropic.com")
					}
					executor := NewClaudeExecutor(&config.Config{})
					req := cliproxyexecutor.Request{Model: "claude-fable-5-2", Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers}
					switch operation {
					case "count":
						if _, err := executor.CountTokens(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
					case "stream":
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					default:
						if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
					if calls != 1 {
						t.Fatalf("upstream calls = %d, want exactly one", calls)
					}
					for _, name := range []string{"User-Agent", "X-Client-App", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "X-Stainless-Os", "X-Stainless-Arch", "X-Claude-Code-Agent-Id"} {
						if got, want := helps.HeaderValueCaseInsensitive(seenHeaders, name), headers.Get(name); got != want {
							t.Errorf("%s = %q, want %q", name, got, want)
						}
					}
					for _, beta := range strings.Split(headers.Get("Anthropic-Beta"), ",") {
						if !strings.Contains(helps.HeaderValueCaseInsensitive(seenHeaders, "Anthropic-Beta"), beta) {
							t.Errorf("lost client beta %q", beta)
						}
					}
					paths := []string{"model", "messages", "tools"}
					if operation != "count" {
						paths = append(paths, "system.1", "thinking", "fallbacks")
					}
					for _, path := range paths {
						if got, want := gjson.GetBytes(seenBody, path).Raw, gjson.GetBytes(payload, path).Raw; got != want {
							t.Errorf("%s changed:\ngot %s\nwant %s", path, got, want)
						}
					}
					if operation != "count" && !strings.HasPrefix(gjson.GetBytes(seenBody, "system.0.text").String(), billing) {
						t.Errorf("client billing identity changed: %s", gjson.GetBytes(seenBody, "system.0.text"))
					}
					if gjson.GetBytes(seenBody, "diagnostics").Exists() {
						t.Error("injected diagnostics into native request")
					}
				})
			}
		}
	}
}
