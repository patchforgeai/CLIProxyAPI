// Package quotareading normalizes upstream quota observations into per-credential
// windows that routing can rank and gate on.
//
// Readings come from three sources: response headers already captured in
// Auth.Quota.Signals, usage bodies fetched through the management api-call
// path, and an idle-credential poller. Parsers are pure; the Store keeps the
// newest observation per window, and Effective merges both at read time.
//
// The package deliberately depends only on primitives so sdk/cliproxy/auth can
// import it without a cycle; it must never import sdk/cliproxy/auth.
package quotareading

import (
	"math"
	"strings"
	"time"
)

// Upstream usage endpoints whose bodies carry quota windows.
const (
	ClaudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	ClaudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	CodexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
)

// Kind classifies how a window takes part in routing.
type Kind int

const (
	// KindShort is a window of at most 24 hours. It only gates.
	KindShort Kind = iota
	// KindLong is a window longer than 24 hours. It ranks credentials.
	KindLong
	// KindScoped is model specific. It gates requests for the matching model family.
	KindScoped
)

// String returns "short", "long" or "scoped".
func (k Kind) String() string {
	switch k {
	case KindShort:
		return "short"
	case KindLong:
		return "long"
	case KindScoped:
		return "scoped"
	default:
		return "unknown"
	}
}

// Source records where a window was observed.
type Source string

const (
	SourceHeader Source = "header"
	SourceUsage  Source = "usage"
	SourcePoll   Source = "poll"
)

// Window is one quota window of one credential at one observation time.
type Window struct {
	// ID identifies the window per credential. Claude: "5h", "7d", "7d:fable",
	// "7d:opus", "7d:sonnet". Codex: "primary", "secondary".
	ID   string
	Kind Kind
	// UsedPercent is clamped to 0..100. NaN is never stored.
	UsedPercent float64
	// ResetsAt is when the window resets. The zero value means unknown.
	ResetsAt time.Time
	// Length is the window duration. Zero means unknown.
	Length time.Duration
	// Model is the lowercased model family for KindScoped ("fable", "opus",
	// "sonnet"); it is empty for other kinds.
	Model      string
	ObservedAt time.Time
	Source     Source
}

// RemainingPercent returns 100 - UsedPercent, clamped to 0..100. A NaN usage,
// which parsers and the store never produce, counts as fully used.
func (w Window) RemainingPercent() float64 {
	if math.IsNaN(w.UsedPercent) {
		return 0
	}
	return clampPercent(100 - w.UsedPercent)
}

// Reading is the set of windows known for one credential, sorted by ID.
type Reading struct {
	AuthID   string
	Provider string
	Windows  []Window
}

// ModelFamily maps a model name to the family used by model-scoped windows.
// It matches lowercased substrings in this order: "fable", "opus", "sonnet",
// "haiku". Any other model returns "".
func ModelFamily(model string) string {
	lower := strings.ToLower(model)
	for _, family := range []string{"fable", "opus", "sonnet", "haiku"} {
		if strings.Contains(lower, family) {
			return family
		}
	}
	return ""
}

// clampPercent limits a finite percentage to 0..100.
func clampPercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

// kindForLength classifies a window by its length: up to 24 hours gates, longer ranks.
func kindForLength(length time.Duration) Kind {
	if length > 24*time.Hour {
		return KindLong
	}
	return KindShort
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
