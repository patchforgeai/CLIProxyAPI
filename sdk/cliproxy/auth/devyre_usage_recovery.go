package auth

// devyre: a fresh subscription reading can lift a rejection before its old
// reset time (for example after a plan is restored or a reset is redeemed).

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
)

// claudeSharedWindows are the windows every model of a Claude subscription
// counts against, by the status header that rejects each.
var claudeSharedWindows = map[string]string{
	"anthropic-ratelimit-unified-5h-status": "5h",
	"anthropic-ratelimit-unified-7d-status": "7d",
}

// claudeUsage is one complete, fresh Claude usage response: every window it
// lists, and the shared windows it reports as limits the account does not have
// (quotareading.ClaudeUsageUnlimited), such as the 5-hour one of a plan
// without it.
type claudeUsage struct {
	windows   []quotareading.Window
	unlimited []string
}

// reconcileUsage lifts the quota cooldowns of the registered credential behind
// observed that a fresh usage response contradicts, and publishes the change
// to both selection paths and the persisted cooldowns. lift decides under the
// manager's lock: it clears what the response contradicts and reports whether
// it cleared anything. started is the upstream call's start, not its
// completion: a response racing a newer rejection must never erase it, so the
// credential must not have changed since observed was read, even on the same
// clock tick. A disabled credential and one whose sign-in failed stay as they
// are.
func (m *Manager) reconcileUsage(observed *Auth, provider string, started time.Time, lift func(auth *Auth, now time.Time) bool) {
	if m == nil || observed == nil || started.IsZero() {
		return
	}
	authID := observed.ID
	now := time.Now()
	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil || auth.Provider != provider || auth.Disabled || auth.Status == StatusDisabled ||
		hasUnauthorizedAuthFailure(auth) || !auth.UpdatedAt.Before(started) ||
		auth.Generation != observed.Generation || auth.RegistrationEpoch != observed.RegistrationEpoch {
		m.mu.Unlock()
		return
	}
	if !lift(auth, now) {
		m.mu.Unlock()
		return
	}
	// Drop the aggregate of the model quotas we just cleared before rebuilding it.
	if auth.Quota.Reason == "quota" {
		applyCooldownFields(&auth.Quota, QuotaState{})
	}
	updateAggregatedAvailability(auth, now)
	if !hasModelError(auth, now) && quotaErrorOrNil(auth.LastError) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.Generation++
	auth.UpdatedAt = now
	snapshot := auth.Clone()
	m.mu.Unlock()

	// Publish the same generation to both selection paths. An execution result
	// that won a race after the unlock takes precedence in each projection.
	models, epoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID)
	projections := make([]registry.ClientModelProjection, 0, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	registry.GetGlobalRegistry().ApplyClientModelProjections(authID, epoch, snapshot.Generation, projections)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	m.persistCooldownStates(context.Background())
}

// liftSharedQuota clears the credential-wide block and returns the deadline it
// held, by which the states it was propagated to are told (propagatedQuota).
func liftSharedQuota(auth *Auth) time.Time {
	deadline := auth.Quota.NextRecoverAt
	applyCooldownFields(&auth.Quota, QuotaState{})
	auth.NextRetryAfter = time.Time{}
	auth.Unavailable = false
	return deadline
}

// propagatedQuota reports whether a model's state holds nothing but the
// credential-wide block a rejection of another model wrote over it: it made no
// rejected request of its own, and its deadline is the block's. Its earlier,
// independent deadlines and its own errors still stand.
func propagatedQuota(state *ModelState, sharedDeadline time.Time) bool {
	return state.LastError == nil && state.Quota.Reason == "credential_quota" &&
		state.Quota.NextRecoverAt.Equal(sharedDeadline) && state.NextRetryAfter.Equal(sharedDeadline)
}

// ReconcileClaudeUsage lifts only subscription-window cooldowns contradicted by
// a complete, fresh usage response. windows are the windows it lists and
// unlimited the shared ones it reports the account does not have. started is
// the upstream call's start (reconcileUsage). Cached responses, profile reads
// and partial usage variants must not call it.
func (m *Manager) ReconcileClaudeUsage(observed *Auth, windows []quotareading.Window, unlimited []string, started time.Time) {
	usage := claudeUsage{windows: windows, unlimited: unlimited}
	if !usage.healthy("") {
		return
	}
	m.reconcileUsage(observed, "claude", started, func(auth *Auth, now time.Time) bool {
		shared := auth.Quota.Reason == "credential_quota" && quotaErrorOrNil(auth.LastError) &&
			claudeSharedQuotaRecovered(auth, usage, started, now)
		changed := shared
		var sharedDeadline time.Time
		if shared {
			sharedDeadline = liftSharedQuota(auth)
		}
		for model, state := range auth.ModelStates {
			if state == nil || state.Status == StatusDisabled || !state.UpdatedAt.Before(started) ||
				!quotaErrorOrNil(state.LastError) || !usage.healthy(model) {
				continue
			}
			// A shared rejection is also propagated to models that made no request.
			if (shared && propagatedQuota(state, sharedDeadline)) || claudeRejectionRecovered(state, usage, started, model, now) {
				resetModelState(state, now)
				changed = true
			}
		}
		return changed
	})
}

// ReconcileCodexUsage lifts the usage-limit cooldowns of a ChatGPT account
// after a complete, fresh usage response: call it for such a response only,
// never for a cached one. room is whether the response reports the account at
// none of its limits (quotareading.CodexUsageHasRoom); without it nothing is
// lifted. started is the upstream call's start (reconcileUsage).
//
// ChatGPT rejects a request of an account at a limit with the error type
// usage_limit_reached, which blocks the whole credential until the reset the
// rejection named (unless upstream.codex.model-level-cooling scopes it to the
// model). A model whose own last result was such a rejection is the evidence
// of the block: the block and that model's cooldown are lifted, with the states
// the block was propagated to. A later per-minute rate limit, a model at
// capacity and any other failure of a model replace that evidence and stand, as
// they say nothing of the account's usage.
func (m *Manager) ReconcileCodexUsage(observed *Auth, room bool, started time.Time) {
	if !room {
		return
	}
	m.reconcileUsage(observed, "codex", started, func(auth *Auth, now time.Time) bool {
		shared := false
		if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && quotaErrorOrNil(auth.LastError) {
			for _, state := range auth.ModelStates {
				if codexUsageLimitRejected(state, started, now) {
					shared = true
					break
				}
			}
		}
		changed := shared
		var sharedDeadline time.Time
		if shared {
			sharedDeadline = liftSharedQuota(auth)
		}
		for _, state := range auth.ModelStates {
			if state == nil || state.Status == StatusDisabled {
				continue
			}
			if (shared && propagatedQuota(state, sharedDeadline)) || codexUsageLimitRejected(state, started, now) {
				resetModelState(state, now)
				changed = true
			}
		}
		return changed
	})
}

// codexUsageLimitRejected reports whether the model's own last result was
// ChatGPT's usage-limit rejection, recorded before the usage call started, and
// its cooldown still runs. That holds for a state a later rejection of another
// model wrote the credential-wide block over too: its own rejection is still
// its last error.
func codexUsageLimitRejected(state *ModelState, started, now time.Time) bool {
	if state == nil || !codexUsageLimitError(state.LastError) || !state.UpdatedAt.Before(started) {
		return false
	}
	quota := state.Quota
	return quota.Exceeded && (quota.Reason == "quota" || quota.Reason == "credential_quota") &&
		quota.NextRecoverAt.After(now)
}

// codexUsageLimitError reports whether err is ChatGPT's refusal of a request
// for the account's usage limit: a 429 whose body names the type
// usage_limit_reached, as the Codex executor reads it (isCodexUsageLimitError).
// A per-minute rate limit and a model at capacity are 429s of another type.
func codexUsageLimitError(err *Error) bool {
	if err == nil || err.HTTPStatus != http.StatusTooManyRequests || !gjson.Valid(err.Message) {
		return false
	}
	for _, path := range []string{"error.type", "type"} {
		if strings.EqualFold(strings.TrimSpace(gjson.Get(err.Message, path).String()), "usage_limit_reached") {
			return true
		}
	}
	return false
}

func quotaErrorOrNil(err *Error) bool {
	return err == nil || err.HTTPStatus == http.StatusTooManyRequests
}

// claudeSharedQuotaRecovered reports whether the usage contradicts the
// credential-wide block: a model's own last result was rejected on a shared
// window, and every such window has room again.
//
// Each rejection of a shared window writes the credential-wide block over the
// other models' states (reason "credential_quota") and keeps the later of the
// deadlines, so when several models were rejected, as the requests in flight
// are when an account runs out, the model holding the block's own deadline is
// not the one whose state still reads "quota". Any model that carries its own
// rejection is evidence of the block, whatever its state reads now. A token
// refresh or a concurrent success can update the account after its rejection;
// the originating model still records that rejection. Conversely, headers kept
// from an older request cannot explain a new ordinary 429: that model's own
// result dropped them (MarkResult).
func claudeSharedQuotaRecovered(auth *Auth, usage claudeUsage, started, now time.Time) bool {
	recovered := false
	for _, state := range auth.ModelStates {
		rejected, ok := claudeRejectedWindows(state, started, "", now)
		if !ok {
			continue
		}
		if !usage.hasRoom(rejected) {
			return false
		}
		recovered = true
	}
	return recovered
}

// claudeRejectionRecovered reports whether the usage contradicts the model's
// own subscription rejection: every window its last result was rejected on has
// room again. That holds for a state a later rejection of another model wrote
// the credential-wide block over too: its own rejection is still what its
// signals record.
func claudeRejectionRecovered(state *ModelState, usage claudeUsage, started time.Time, model string, now time.Time) bool {
	rejected, ok := claudeRejectedWindows(state, started, model, now)
	return ok && usage.hasRoom(rejected)
}

// claudeRejectedWindows lists the windows the model's own last result was
// rejected on: the shared ones, and for a model with a weekly window of its own
// (Fable) that one. ok is false when the state records no such rejection that
// still holds: its cooldown is over, it is not a quota cooldown, or its signals
// are not those of its last result, observed before the usage call started.
func claudeRejectedWindows(state *ModelState, started time.Time, model string, now time.Time) (rejected []string, ok bool) {
	if state == nil || !quotaErrorOrNil(state.LastError) {
		return nil, false
	}
	quota := state.Quota
	if !quota.Exceeded || (quota.Reason != "quota" && quota.Reason != "credential_quota") ||
		!quota.NextRecoverAt.After(now) || quota.ObservedAt.IsZero() ||
		!quota.ObservedAt.Equal(state.UpdatedAt) || !quota.ObservedAt.Before(started) {
		return nil, false
	}
	family := quotareading.ModelFamily(model)
	for name, status := range quota.Signals {
		if !strings.EqualFold(status, "rejected") {
			continue
		}
		name = strings.ToLower(name)
		if id, shared := claudeSharedWindows[name]; shared {
			rejected = append(rejected, id)
		} else if name == "anthropic-ratelimit-unified-7d_oi-status" && family == "fable" {
			rejected = append(rejected, "7d:fable")
		}
	}
	return rejected, len(rejected) > 0
}

// healthy reports whether every window that applies to model has room, and the
// response accounts for both shared windows: each is listed, or reported as a
// limit the account does not have, and at least one is listed. A malformed or
// incomplete response is not evidence of recovery, even when the windows it
// does contain are empty.
func (u claudeUsage) healthy(model string) bool {
	family := quotareading.ModelFamily(model)
	short, long := false, false
	for _, window := range u.windows {
		if window.Kind == quotareading.KindScoped && (family == "" || window.Model != family) {
			continue
		}
		if !windowHasRoom(window) {
			return false
		}
		short = short || window.ID == "5h"
		long = long || window.ID == "7d"
	}
	return (short || long) && (short || u.unlimitedWindow("5h")) && (long || u.unlimitedWindow("7d"))
}

// hasRoom reports whether every window in ids has room again: listed below its
// limit, or a shared window the account no longer has. A model's own window
// must be listed: one the response leaves out says nothing of it.
func (u claudeUsage) hasRoom(ids []string) bool {
	for _, id := range ids {
		if !u.windowHasRoom(id) {
			return false
		}
	}
	return true
}

func (u claudeUsage) windowHasRoom(id string) bool {
	for _, window := range u.windows {
		if window.ID == id {
			return windowHasRoom(window)
		}
	}
	return u.unlimitedWindow(id)
}

func (u claudeUsage) unlimitedWindow(id string) bool {
	for _, unlimited := range u.unlimited {
		if unlimited == id {
			return true
		}
	}
	return false
}

func windowHasRoom(window quotareading.Window) bool {
	return !math.IsNaN(window.UsedPercent) && !math.IsInf(window.UsedPercent, 0) &&
		window.UsedPercent >= 0 && window.UsedPercent < 100
}
