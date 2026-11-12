package auth

// devyre: the log says when a limit holds a credential back, on which windows
// and until when, and when the hold is lifted or kept.
//
// Upstream logs a rejection as "upstream execution failed ... err=<body>" and,
// for Claude, "parsed Anthropic rate limit reset headers", whose windows and
// deadline are fields the log formatter does not print (it prints a fixed list
// of field names). Neither says what the router did with the rejection: whether
// the whole credential or one model is held back, on which limit, until when.
// And nothing at all said that a hold was lifted, or why a fresh usage reading
// left it standing. A report of sessions that fail for usage could not be read
// from the log. These lines carry it in their message, as the selector's do:
//
//	quota: held | provider=claude auth=<identity> model=claude-opus-5-5 scope=credential rejected=7d claim=seven_day until=2026-11-14T10:00:00Z in=71h59m0s
//	quota: back in use | provider=claude auth=<identity> by=usage
//	quota: back in use | provider=codex auth=<identity> by=reset
//	quota: hold kept | provider=claude auth=<identity> why="its usage does not show room at every limit"

import (
	"context"
	"net/http"
	"strings"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	log "github.com/sirupsen/logrus"
)

// Why a fresh usage response left a hold standing (devyreLogQuotaKept).
const (
	quotaKeptNoRoom     = "its usage does not show room at every limit"
	quotaKeptChanged    = "it changed while its usage was read"
	quotaKeptNoEvidence = "its usage contradicts no rejection on record"
)

// claudeRejectionHeaders are the limit windows a Claude rejection can name, by
// the status header that names each, under the names the readings give them.
var claudeRejectionHeaders = []struct{ header, window string }{
	{"Anthropic-Ratelimit-Unified-5h-Status", "5h"},
	{"Anthropic-Ratelimit-Unified-7d-Status", "7d"},
	{"Anthropic-Ratelimit-Unified-7d_oi-Status", "7d:fable"},
	{"Anthropic-Ratelimit-Unified-Overage-Status", "overage"},
}

// devyreLogQuotaHold says that a 429 put a credential, or one of its models, in
// a quota cooldown: which limit windows the provider rejected it on, and until
// when it is held back. auth is the credential as the result left it. Nothing
// is said of a 429 that held nothing back (cooling turned off, a refusal that
// belongs to the request).
func devyreLogQuotaHold(ctx context.Context, result Result, auth *Auth, modelKey string) {
	if auth == nil || result.Success || statusCodeFromResult(result.Error) != http.StatusTooManyRequests {
		return
	}
	now := time.Now()
	hold, scope := auth.Quota, "credential"
	if !result.CredentialScope || hold.Reason != "credential_quota" {
		state := existingModelState(auth, modelKey)
		if state == nil {
			return
		}
		hold, scope = state.Quota, "model"
	}
	if !hold.Exceeded || !hold.NextRecoverAt.After(now) {
		return
	}
	headers := internallogging.GetResponseHeaders(ctx)
	claim := ""
	if strings.EqualFold(strings.TrimSpace(result.Provider), "claude") {
		if value := logSafeToken(headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim")); value != "" {
			claim = " claim=" + value
		}
	}
	model := logSafeToken(modelKey)
	if model == "" {
		model = "-"
	}
	logEntryWithRequestID(ctx).Infof("quota: held | provider=%s auth=%s model=%s scope=%s rejected=%s%s until=%s in=%s",
		result.Provider, formatAuthIdentity(auth, result.Provider), model, scope,
		devyreRejectedWindows(result, headers, now), claim,
		hold.NextRecoverAt.UTC().Format(time.RFC3339), hold.NextRecoverAt.Sub(now).Round(time.Second))
}

// devyreRejectedWindows names the limit windows a rejection reports at their
// limit, as the quota readings name them: Claude's by the status headers that
// say "rejected" ("5h", "7d", "7d:fable", "overage"; "unified" when only the
// overall status does), ChatGPT's by the windows its headers report full
// ("primary", "secondary"), else "usage_limit" for its usage-limit error. A
// rejection that names none, an ordinary rate limit, reads "none".
func devyreRejectedWindows(result Result, headers http.Header, now time.Time) string {
	var windows []string
	switch strings.ToLower(strings.TrimSpace(result.Provider)) {
	case "claude":
		for _, named := range claudeRejectionHeaders {
			if strings.EqualFold(strings.TrimSpace(headers.Get(named.header)), "rejected") {
				windows = append(windows, named.window)
			}
		}
		if len(windows) == 0 && strings.EqualFold(strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-Status")), "rejected") {
			windows = append(windows, "unified")
		}
	case "codex":
		for _, window := range quotareading.FromCodexHeaders(collectQuotaSignals("codex", headers), now) {
			if window.UsedPercent >= 100 {
				windows = append(windows, window.ID)
			}
		}
		if len(windows) == 0 && codexUsageLimitError(result.Error) {
			windows = append(windows, "usage_limit")
		}
	}
	if len(windows) == 0 {
		return "none"
	}
	return strings.Join(windows, ",")
}

// DevyreLogQuotaLifted says that a credential a limit held back takes requests
// again, and by what: "usage" for a fresh usage response that contradicted the
// rejection (reconcileUsage), "reset" for a cooldown reset a client asked for.
func DevyreLogQuotaLifted(auth *Auth, by string) {
	if auth == nil {
		return
	}
	log.Infof("quota: back in use | provider=%s auth=%s by=%s", auth.Provider, formatAuthIdentity(auth, auth.Provider), by)
}

// devyreLogQuotaKept says why a fresh usage response left a credential's hold
// standing, when it holds one. Without it nothing in the log told a hold that
// was read and kept from one that was never read.
func devyreLogQuotaKept(auth *Auth, why string, now time.Time) {
	if auth == nil || !quotaHoldRuns(auth, now) {
		return
	}
	log.Infof("quota: hold kept | provider=%s auth=%s why=%q", auth.Provider, formatAuthIdentity(auth, auth.Provider), why)
}

// quotaHoldRuns reports whether a quota cooldown of the credential, or of one
// of its models, still runs.
func quotaHoldRuns(auth *Auth, now time.Time) bool {
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		return true
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) {
			return true
		}
	}
	return false
}

// logSafeToken returns value when it is a short name of letters, digits,
// ".", "_", "-" and ":", else "": what a provider sends never breaks a log line.
func logSafeToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 96 {
		return ""
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') ||
			char == '.' || char == '_' || char == '-' || char == ':' {
			continue
		}
		return ""
	}
	return value
}
