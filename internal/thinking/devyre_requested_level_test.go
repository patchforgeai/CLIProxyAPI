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
