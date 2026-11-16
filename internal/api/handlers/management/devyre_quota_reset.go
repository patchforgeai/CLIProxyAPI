package management

// devyre: a cooldown reset also forgets what was read of the credential's
// quota before it, and may say when the quota started over.
//
// POST /v8/management/routing/cooldown/reset (and /v0/management/reset-quota)
// says a credential's quota started over: its provider granted a reset, which
// a client redeemed through api-call, or an operator cleared a cooldown they
// know to be wrong. Upstream clears the conductor's cooldown. The readings the
// selector gates and ranks by would still say the credential is exhausted: the
// rejection's own headers read "rejected", and nothing replaces them until the
// credential serves a request or its usage is read, which the provider may
// refuse for minutes after a reset (Claude's usage endpoint is tightly rate
// limited). So the reset also drops every reading of the credential from
// before it, and takes its cached usage for stale, as a write through api-call
// does: the next usage read goes upstream, and one still in flight from before
// the reset records nothing.
//
// The body may name when the quota started over, `observed_before` (RFC 3339):
// a reset used on another computer of the account reaches this router as soon
// as it can be told, which for a computer that was away is once it is back.
// A quota rejection the provider made after that time is newer news than the
// reset (the credential ran out again): the reset then leaves every hold as it
// is and answers `"status": "kept"`. Otherwise it clears the holds and drops the
// readings observed up to that time; later ones are newer than the reset and
// stand. Without the field the reset is now, as before.

import (
	"errors"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// errResetTime is a reset's `observed_before` that is not an RFC 3339 time.
var errResetTime = errors.New("observed_before must be an RFC 3339 time")

// devyreResetAt reads a cooldown reset's `observed_before`: when the
// credential's quota started over, never later than now; now when the request
// names none. kept is whether the provider refused the credential on its quota
// since then, which leaves its holds standing.
func (h *Handler) devyreResetAt(auth *coreauth.Auth, observedBefore string) (at time.Time, kept bool, err error) {
	now := h.devyreUsageCache().now()
	raw := strings.TrimSpace(observedBefore)
	if raw == "" {
		return now, false, nil
	}
	at, errParse := time.Parse(time.RFC3339Nano, raw)
	if errParse != nil {
		return time.Time{}, false, errResetTime
	}
	if at.After(now) {
		at = now
	}
	if auth != nil && h.authManager != nil && h.authManager.DevyreRejectedSince(auth.ID, at) {
		coreauth.DevyreLogResetKept(auth)
		return at, true, nil
	}
	return at, false, nil
}

// devyreQuotaReset forgets the readings of a credential whose cooldown was
// just reset, as they stood at `at` (devyreResetAt), and its cached usage.
func (h *Handler) devyreQuotaReset(auth *coreauth.Auth, at time.Time) {
	if h == nil || auth == nil || auth.ID == "" {
		return
	}
	cache := h.devyreUsageCache()
	now := cache.now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	cache.invalidate(strings.TrimSpace(auth.Index), now)
	cache.store().Reset(auth.ID, at)
	coreauth.DevyreLogQuotaLifted(auth, "reset")
}
