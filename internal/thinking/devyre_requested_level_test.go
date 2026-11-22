package thinking

import "testing"

func TestDevyreRequestedLevel(t *testing.T) {
	for _, tc := range []struct {
		name, body, format, model, want string
	}{
		{"gemini level", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`, "gemini", "gemini-4-neon", "low"},
		{"gemini snake case", `{"generationConfig":{"thinkingConfig":{"thinking_level":"high"}}}`, "gemini", "gemini-4-neon", "high"},
		{"gemini budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`, "gemini", "gemini-3.1-pro-preview", ""},
		{"gemini auto", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"auto"}}}`, "gemini", "gemini-4-neon", ""},
		{"gemini none", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"none"}}}`, "gemini", "gemini-4-neon", ""},
		{"no thinking config", `{"contents":[]}`, "gemini", "gemini-4-neon", ""},
		{"antigravity envelope", `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"XHIGH"}}}}`, "antigravity", "gemini-4-neon", "xhigh"},
		{"openai chat", `{"reasoning_effort":"medium"}`, "openai", "gemini-4-neon", "medium"},
		{"responses", `{"reasoning":{"effort":"high"}}`, "openai-response", "gemini-4-neon", "high"},
		{"claude effort", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, "claude", "gemini-4-neon", "low"},
		{"claude budget", `{"thinking":{"type":"enabled","budget_tokens":4096}}`, "claude", "gemini-4-neon", ""},
		{"suffix wins", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`, "gemini", "gemini-4-neon(medium)", "medium"},
		{"budget suffix names none", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`, "gemini", "gemini-4-neon(8192)", ""},
		{"invalid body", `not json`, "gemini", "gemini-4-neon", ""},
	} {
		if got := DevyreRequestedLevel([]byte(tc.body), tc.format, tc.model); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDevyreSetRequestedLevel(t *testing.T) {
	for _, tc := range []struct {
		name, body, format, level, want string
	}{
		{"gemini capitals kept", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"XHIGH"}}}`, "gemini", "high", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`},
		{"gemini lower case", `{"generationConfig":{"thinkingConfig":{"thinking_level":"xhigh"}}}`, "gemini", "high", `{"generationConfig":{"thinkingConfig":{"thinking_level":"high"}}}`},
		{"antigravity envelope", `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"MINIMAL"}}}}`, "antigravity", "low", `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}}`},
		{"openai", `{"reasoning_effort":"xhigh"}`, "openai", "high", `{"reasoning_effort":"high"}`},
		{"responses", `{"reasoning":{"effort":"xhigh"}}`, "openai-response", "high", `{"reasoning":{"effort":"high"}}`},
		{"claude", `{"output_config":{"effort":"max"}}`, "claude", "high", `{"output_config":{"effort":"high"}}`},
		{"removed", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"XHIGH","includeThoughts":true}}}`, "gemini", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`},
		{"no level named", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`, "gemini", "high", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`},
		{"invalid body", `not json`, "gemini", "high", `not json`},
	} {
		body := []byte(tc.body)
		got := DevyreSetRequestedLevel(body, tc.format, tc.level)
		if string(got) != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.name, got, tc.want)
		}
		if string(body) != tc.body {
			t.Fatalf("%s: the request's own body was changed: %s", tc.name, body)
		}
	}
}
