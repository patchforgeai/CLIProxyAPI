package thinking

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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

// devyreLevelPaths are the fields each source format names a thinking level
// in, the ones DevyreRequestedLevel reads.
var devyreLevelPaths = map[string][]string{
	"gemini":          {"generationConfig.thinkingConfig.thinkingLevel", "generationConfig.thinkingConfig.thinking_level"},
	"antigravity":     {"request.generationConfig.thinkingConfig.thinkingLevel", "request.generationConfig.thinkingConfig.thinking_level"},
	"openai":          {"reasoning_effort"},
	"codex":           {"reasoning.effort"},
	"openai-response": {"reasoning.effort"},
	"claude":          {"output_config.effort"},
	"interactions": {
		"generation_config.thinking_level",
		"generation_config.thinkingLevel",
		"generation_config.thinking_config.thinking_level",
		"generation_config.thinking_config.thinkingLevel",
		"generation_config.thinkingConfig.thinking_level",
		"generation_config.thinkingConfig.thinkingLevel",
	},
}

// DevyreSetRequestedLevel returns a copy of body whose thinking level, in
// each field the source format names one in, is level, written in capitals
// where the request wrote it so (Gemini's "HIGH"); an empty level removes the
// fields (devyre). body itself is never changed: a request is sent again on
// another credential as it came. A body naming no level comes back as it is.
//
// The Antigravity executor uses it when the variant a request reaches serves
// another level than the one asked for (internal/antigravityaccountmodels).
func DevyreSetRequestedLevel(body []byte, format, level string) []byte {
	paths := devyreLevelPaths[strings.ToLower(strings.TrimSpace(format))]
	if len(paths) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	out := body
	copied := false
	for _, path := range paths {
		current := gjson.GetBytes(out, path)
		if !current.Exists() || current.Type != gjson.String {
			continue
		}
		if !copied {
			out = bytes.Clone(body)
			copied = true
		}
		var errSet error
		var next []byte
		if level == "" {
			next, errSet = sjson.DeleteBytes(out, path)
		} else {
			value := strings.ToLower(level)
			if original := current.String(); original == strings.ToUpper(original) && original != strings.ToLower(original) {
				value = strings.ToUpper(value)
			}
			next, errSet = sjson.SetBytes(out, path, value)
		}
		if errSet == nil {
			out = next
		}
	}
	return out
}
