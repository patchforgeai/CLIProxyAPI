package helps

import (
	"net/http"
	"testing"
)

// The envelope captured from Claude Code 2.1.321 launched by Agent SDK 0.3.296.
// Recognition is a wire-format decision, not an allowlist of client releases.
func TestDevyreClaudeAgentSDKDetection(t *testing.T) {
	for _, version := range []string{"2.1.257", "2.1.280", "2.1.321", "2.2.0", "3.0.0"} {
		for _, clientApp := range []string{"", ", client-app/anvil/0.0.0-probe"} {
			for _, countTokens := range []bool{false, true} {
				headers := confirmedClaudeCodeHeaders()
				headers.Set("User-Agent", "claude-cli/"+version+" (external, sdk-ts, agent-sdk/0.3.296"+clientApp+")")
				payload := claudeCodeDetectionPayload(validClaudeCodeMetadataUserID)
				if countTokens {
					payload = []byte(`{"model":"claude-fable-5-2","messages":[]}`)
				}
				if got := DetectClaudeCodeRequest(headers, payload, countTokens); !got.Confirmed || got.Entrypoint != "sdk-ts" {
					t.Errorf("version=%s app=%q count=%v: detection = %#v", version, clientApp, countTokens, got)
				}
			}
		}
	}
}

func TestDevyreClaudeAgentSDKStillRequiresNativeEnvelope(t *testing.T) {
	const ua = "claude-cli/2.1.321 (external, sdk-ts, agent-sdk/0.3.296, client-app/anvil/1.0)"
	for _, missing := range []string{"User-Agent", "X-App", "Anthropic-Beta", "metadata"} {
		headers := confirmedClaudeCodeHeaders()
		headers.Set("User-Agent", ua)
		payload := claudeCodeDetectionPayload(validClaudeCodeMetadataUserID)
		if missing == "metadata" {
			payload = []byte(`{}`)
		} else {
			headers.Del(missing)
		}
		if got := DetectClaudeCodeRequest(headers, payload, false); got.Confirmed {
			t.Errorf("missing %s: confirmed incomplete envelope", missing)
		}
	}
	for _, malformed := range []string{
		"claude-cli/2.1.321 (external, sdk-ts, agent-sdk/0.3.296, client-app/)",
		"claude-cli/2.1.321 (external, sdk-ts, agent-sdk/0.3.296, client-app/anvil) trailing",
		"claude-cli/2.1.321 (external, sdk-ts, agent-sdk/invalid)",
		"claude-cli/2.1.321 (external, sdk-ts)",
		"claude-cli/2.1.321 (external, unknown, agent-sdk/0.3.296, client-app/anvil)",
	} {
		headers := confirmedClaudeCodeHeaders()
		headers.Set("User-Agent", malformed)
		if got := DetectClaudeCodeRequest(headers, claudeCodeDetectionPayload(validClaudeCodeMetadataUserID), false); got.Confirmed {
			t.Errorf("confirmed malformed envelope %q", malformed)
		}
	}
}

func TestDevyreClaudeAgentSDKPreservesSoftwareVersions(t *testing.T) {
	incoming := http.Header{}
	for name, value := range map[string]string{
		"User-Agent":                  "claude-cli/2.1.321 (external, sdk-ts, agent-sdk/0.3.296, client-app/anvil/1.0)",
		"X-Stainless-Package-Version": "0.128.0",
		"X-Stainless-Runtime-Version": "v26.12.1",
		"X-Stainless-Os":              "Windows",
		"X-Stainless-Arch":            "x64",
	} {
		incoming.Set(name, value)
	}
	request, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	ApplyClaudeLegacyDeviceHeaders(request, incoming, nil, true)
	for name := range incoming {
		if got, want := request.Header.Get(name), incoming.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
