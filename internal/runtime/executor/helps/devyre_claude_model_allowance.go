package helps

// devyre: a rejection on a model's own weekly allowance holds that model alone.
//
// Anthropic names the limit that refused a request in its representative claim. Three claims are a
// model's own weekly allowance: seven_day_overage_included (Fable's), seven_day_opus and
// seven_day_sonnet. Claude Code 2.1.325 calls them the "Fable limit", "Opus limit" and "Sonnet
// limit", and when one is used up it offers to switch off the model: the account's other models
// still serve. Upstream reads such a rejection as the model's own only when the 429 also confirms
// both shared windows, the 5-hour and the weekly one, below their limit (a status of allowed, or
// a utilization below 1). A 429 that leaves out a shared window's status and utilization, as
// Anthropic does for a window it says nothing of, therefore held the whole credential, every
// model, until the allowance's reset: on 2026-11-16 six of seven accounts stopped serving Opus
// and Sonnet for days on their Fable allowance alone, while their shared windows had room.
//
// A shared window the 429 reports at its limit (rejected, or a utilization of 1 or more) or
// reports in a way that cannot be read keeps the whole credential held, as upstream holds it.

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
)

// devyreModelAllowanceRejection reports whether a 429 refused the request on the requested model's
// own weekly allowance and on nothing every model shares: its representative claim names such an
// allowance (quotareading.ClaudeAllowanceFamily), and neither shared window is reported at or past
// its limit. status5h and status7d are the lowercased status headers of the 5-hour and weekly
// windows.
func devyreModelAllowanceRejection(headers http.Header, status5h, status7d string) bool {
	if quotareading.ClaudeAllowanceFamily(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-Representative-Claim")) == "" {
		return false
	}
	return claudeSharedWindowNotFull(status5h, getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-5h-Utilization")) &&
		claudeSharedWindowNotFull(status7d, getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d-Utilization"))
}

// claudeSharedWindowNotFull reports whether a 429 says nothing of a shared window at its limit:
// the window's status is allowed, or it is absent and the utilization is below 1 or absent too (a
// window the response does not report on). A rejected window, a utilization of 1 or more, one that
// cannot be read, and a status this code does not know are not read as room.
func claudeSharedWindowNotFull(status, utilization string) bool {
	if isClaudeWindowAllowed(status) {
		return true
	}
	if status != "" {
		return false
	}
	if strings.TrimSpace(utilization) == "" {
		return true
	}
	return isClaudeUtilizationHealthy(utilization)
}
