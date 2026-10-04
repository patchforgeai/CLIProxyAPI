package quotareading

import (
	"testing"
	"time"
)

// The routing selector, usage cache, poller and readings endpoint are written
// against this exact API. These assignments fail to compile if it drifts.
var (
	_ func(map[string]string, time.Time) []Window                                   = FromClaudeHeaders
	_ func([]byte, time.Time) ([]Window, error)                                     = FromClaudeUsage
	_ func(map[string]string, time.Time) []Window                                   = FromCodexHeaders
	_ func([]byte, time.Time) ([]Window, error)                                     = FromCodexUsage
	_ func(string, map[string]string, time.Time) []Window                           = FromSignals
	_ func(string, string, []byte, time.Time) ([]Window, error)                     = FromUsageBody
	_ func() *Store                                                                 = NewStore
	_ func() *Store                                                                 = Default
	_ func(*Store, string, string, []Window)                                        = (*Store).Put
	_ func(*Store, string) Reading                                                  = (*Store).Get
	_ func(*Store, string)                                                          = (*Store).Forget
	_ func(*Store) []Reading                                                        = (*Store).Snapshot
	_ func(*Store, string, string, map[string]string, time.Time, time.Time) Reading = Effective
	_ func(string) string                                                           = ModelFamily
	_ func(Reading, string, time.Time, float64) Evaluation                          = Evaluate
	_ func(Window) float64                                                          = Window.RemainingPercent
	_ func(Kind) string                                                             = Kind.String
)

var (
	_ = Window{ID: "", Kind: KindShort, UsedPercent: 0, ResetsAt: time.Time{}, Length: time.Duration(0), Model: "", ObservedAt: time.Time{}, Source: SourceHeader}
	_ = Reading{AuthID: "", Provider: "", Windows: []Window(nil)}
	_ = Evaluation{Usable: false, GateReason: "", GateResetsAt: time.Time{}, UrgencyKnown: false, Urgency: float64(0), RankWindowID: ""}
)

func TestQuotaReadingAPIContractValues(t *testing.T) {
	if KindShort != 0 || KindLong != 1 || KindScoped != 2 {
		t.Fatal("Kind values changed")
	}
	if SourceHeader != "header" || SourceUsage != "usage" || SourcePoll != "poll" {
		t.Fatal("Source values changed")
	}
	if ClaudeUsageURL != "https://api.anthropic.com/api/oauth/usage" ||
		ClaudeProfileURL != "https://api.anthropic.com/api/oauth/profile" ||
		CodexUsageURL != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatal("usage endpoint URLs changed")
	}
}
