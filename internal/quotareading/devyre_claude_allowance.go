package quotareading

import "strings"

// devyre: the representative claims of a Claude 429 that name a model's own weekly allowance.

// claudeAllowanceClaims maps each claim that names a model's own weekly allowance to the model
// family it holds: seven_day_overage_included is Fable's (its window is 7d_oi, "7d:fable"), and
// Claude Code 2.1.325 calls the three the "Fable limit", "Opus limit" and "Sonnet limit".
var claudeAllowanceClaims = map[string]string{
	"seven_day_overage_included": "fable",
	"seven_day_opus":             "opus",
	"seven_day_sonnet":           "sonnet",
}

// ClaudeAllowanceFamily returns the model family whose own weekly allowance a Claude
// representative claim (anthropic-ratelimit-unified-representative-claim) names, or "" for a claim
// on a limit every model of the account counts against (five_hour, seven_day), on overage, or
// none.
func ClaudeAllowanceFamily(claim string) string {
	return claudeAllowanceClaims[strings.ToLower(strings.TrimSpace(claim))]
}
