package thinking

import "strings"

// DevyreRequestedLevel returns the thinking level a request names, in lower
// case (devyre): its model suffix's (gemini-4-neon(low)) when the model has a
// suffix, else the level of its thinking config in the source format (Gemini's
// thinkingLevel, OpenAI's reasoning_effort, the Responses API's
// reasoning.effort, Claude's output_config.effort). A budget, "auto" and
// "none" name no level, and a suffix that names none leaves the body unread,
// since a suffix takes precedence over the body when thinking is applied too.
//
// The Antigravity executor uses it to pick the variant of a model family for
// the level a client asked for (internal/antigravityaccountmodels).
func DevyreRequestedLevel(body []byte, format, model string) string {
	if suffix := ParseSuffix(model); suffix.HasSuffix {
		level, ok := ParseLevelSuffix(strings.TrimSpace(suffix.RawSuffix))
		if !ok {
			return ""
		}
		return string(level)
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "openai-response" {
		format = "codex"
	}
	config := extractThinkingConfig(body, format)
	if config.Mode != ModeLevel {
		return ""
	}
	switch level := strings.ToLower(strings.TrimSpace(string(config.Level))); level {
	case "", string(LevelNone), string(LevelAuto):
		return ""
	default:
		return level
	}
}
