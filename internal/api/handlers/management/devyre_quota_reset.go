package management

// devyre: a cooldown reset also forgets what was read of the credential's
// quota before it.
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

import (
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyreQuotaReset forgets the readings and the cached usage of a credential
// whose cooldown was just reset.
func (h *Handler) devyreQuotaReset(auth *coreauth.Auth) {
	if h == nil || auth == nil || auth.ID == "" {
		return
	}
	cache := h.devyreUsageCache()
	now := cache.now()
	cache.invalidate(strings.TrimSpace(auth.Index), now)
	cache.store().Reset(auth.ID, now)
	coreauth.DevyreLogQuotaLifted(auth, "reset")
}
