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
)

// ReconcileClaudeUsage lifts only subscription-window cooldowns contradicted by
// a complete, fresh usage response. started is the upstream call's start, not
// its completion: a response racing a newer rejection must never erase it.
// Cached responses, profile reads and partial usage variants must not call it.
func (m *Manager) ReconcileClaudeUsage(observed *Auth, windows []quotareading.Window, started time.Time) {
	if m == nil || observed == nil || started.IsZero() || !claudeUsageHealthy(windows, "") {
		return
	}
	authID := observed.ID
	now := time.Now()
	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil || auth.Provider != "claude" || auth.Disabled || auth.Status == StatusDisabled ||
		hasUnauthorizedAuthFailure(auth) || !auth.UpdatedAt.Before(started) ||
		auth.Generation != observed.Generation || auth.RegistrationEpoch != observed.RegistrationEpoch {
		m.mu.Unlock()
		return
	}
	shared := auth.Quota.Reason == "credential_quota" && quotaErrorOrNil(auth.LastError) &&
		claudeSharedQuotaRecovered(auth, windows, started)
	sharedDeadline := auth.Quota.NextRecoverAt
	changed := shared
	if shared {
		applyCooldownFields(&auth.Quota, QuotaState{})
		auth.NextRetryAfter = time.Time{}
		auth.Unavailable = false
	}
	for model, state := range auth.ModelStates {
		if state == nil || state.Status == StatusDisabled || !state.UpdatedAt.Before(started) ||
			!quotaErrorOrNil(state.LastError) || !claudeUsageHealthy(windows, model) {
			continue
		}
		// A shared rejection is also propagated to models that made no request.
		// Their earlier, independent deadlines and non-quota errors still stand.
		propagated := shared && state.LastError == nil && state.Quota.Reason == "credential_quota" &&
			state.Quota.NextRecoverAt.Equal(sharedDeadline) && state.NextRetryAfter.Equal(sharedDeadline)
		if propagated || (state.Quota.Reason == "quota" && state.Quota.ObservedAt.Equal(state.UpdatedAt) && claudeQuotaRecovered(state.Quota, windows, started, model)) {
			resetModelState(state, now)
			changed = true
		}
	}
	if !changed {
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

func quotaErrorOrNil(err *Error) bool {
	return err == nil || err.HTTPStatus == http.StatusTooManyRequests
}

func claudeSharedQuotaRecovered(auth *Auth, windows []quotareading.Window, started time.Time) bool {
	// A token refresh or a concurrent success can update the account after its
	// rejection. The originating model still records that rejection. Conversely,
	// headers kept from an older request cannot explain a new ordinary 429.
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Reason == "quota" && quotaErrorOrNil(state.LastError) &&
			state.Quota.NextRecoverAt.Equal(auth.Quota.NextRecoverAt) &&
			state.Quota.ObservedAt.Equal(state.UpdatedAt) &&
			claudeQuotaRecovered(state.Quota, windows, started, "") {
			return true
		}
	}
	return false
}

// Require both shared windows. A malformed or incomplete response is not
// evidence of recovery, even when the windows it does contain are empty.
func claudeUsageHealthy(windows []quotareading.Window, model string) bool {
	family := quotareading.ModelFamily(model)
	short, long := false, false
	for _, window := range windows {
		if window.Kind == quotareading.KindScoped && (family == "" || window.Model != family) {
			continue
		}
		if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 || window.UsedPercent >= 100 {
			return false
		}
		short = short || window.ID == "5h"
		long = long || window.ID == "7d"
	}
	return short && long
}

func claudeQuotaRecovered(quota QuotaState, windows []quotareading.Window, started time.Time, model string) bool {
	if !quota.Exceeded || (quota.Reason != "quota" && quota.Reason != "credential_quota") ||
		quota.ObservedAt.IsZero() || !quota.ObservedAt.Before(started) {
		return false
	}
	family := quotareading.ModelFamily(model)
	rejected := false
	for name, status := range quota.Signals {
		if !strings.EqualFold(status, "rejected") {
			continue
		}
		id := ""
		switch strings.ToLower(name) {
		case "anthropic-ratelimit-unified-5h-status":
			id = "5h"
		case "anthropic-ratelimit-unified-7d-status":
			id = "7d"
		case "anthropic-ratelimit-unified-7d_oi-status":
			if family == "fable" {
				id = "7d:fable"
			}
		}
		if id == "" {
			continue
		}
		found := false
		for _, window := range windows {
			if window.ID == id && window.UsedPercent >= 0 && window.UsedPercent < 100 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		rejected = true
	}
	return rejected
}
