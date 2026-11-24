package auth

// devyre: a request that no credential can take, while each one that takes it again in time is
// held for a limit, is refused as that limit: the model cooldown (429 model_cooldown, with when
// the first hold ends), whatever keeps the other credentials out until someone acts (signed out,
// an access token expired with no refresh to come, turned off).
//
// Upstream refuses with the model cooldown only when every candidate cools down, and otherwise
// with 503 "no auth available". So one signed-out account beside accounts at their limit made a
// usage limit read as a server outage: Claude Code retried the 503 ten times and ended the turn
// with "Claude's API returned a server error", and a client that waits for usage only after a
// limit (Anvil's sessions) left the session failed when the limit lifted. Seen on 2026-11-24: four
// of seven Claude accounts signed out on one computer, the other three at Fable's weekly limit.
//
// A credential the provider refused the sign-in of (a 401, an invalid grant) is out until it is
// signed in again, though the router tries it again every 30 minutes: each try meets the same
// refusal, and signing in again changes its tokens, which clears it (Manager.Update). A credential
// that comes back by itself (another failure's retry time) is no limit and not out: beside one,
// the refusal stays the 503 it was.

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// devyreLimitSummary is what a refusal is decided by, over the candidates a request had.
type devyreLimitSummary struct {
	total      int
	cooling    int
	outForGood int
	// When the first hold of a cooling candidate ends.
	firstHoldEnds time.Time
}

// add counts one candidate, blocked for `model` as isAuthBlockedForModel says.
func (s *devyreLimitSummary) add(auth *Auth, model string, blocked bool, reason blockReason, next time.Time) {
	s.total++
	switch {
	case !blocked:
	case reason == blockReasonCooldown:
		s.cooling++
		if !next.IsZero() && (s.firstHoldEnds.IsZero() || next.Before(s.firstHoldEnds)) {
			s.firstHoldEnds = next
		}
	case next.IsZero() || devyreSignedOut(auth, model):
		s.outForGood++
	}
}

// merge adds another shard's summary (the mixed-provider scheduler's).
func (s *devyreLimitSummary) merge(other devyreLimitSummary) {
	s.total += other.total
	s.cooling += other.cooling
	s.outForGood += other.outForGood
	if !other.firstHoldEnds.IsZero() && (s.firstHoldEnds.IsZero() || other.firstHoldEnds.Before(s.firstHoldEnds)) {
		s.firstHoldEnds = other.firstHoldEnds
	}
}

// refusedAsLimit reports whether the request is refused as the limit that holds its candidates
// back: at least one cools down until a known time, and every other one is out for good.
func (s devyreLimitSummary) refusedAsLimit() bool {
	return s.cooling > 0 && s.cooling+s.outForGood == s.total && !s.firstHoldEnds.IsZero()
}

// devyreLimitOf summarizes the conductor's candidates for a request of routeModel.
func (m *Manager) devyreLimitOf(auths []*Auth, routeModel string, now time.Time) devyreLimitSummary {
	var summary devyreLimitSummary
	for _, candidate := range auths {
		model := m.selectionModelForAuth(candidate, routeModel)
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		summary.add(candidate, model, blocked, reason, next)
	}
	return summary
}

// devyreLimitLocked summarizes the shard's candidates (those predicate keeps) as the scheduler
// holds them.
func (m *modelScheduler) devyreLimitLocked(predicate func(*scheduledAuth) bool) devyreLimitSummary {
	var summary devyreLimitSummary
	if m == nil {
		return summary
	}
	for _, entry := range m.entries {
		if predicate != nil && !predicate(entry) {
			continue
		}
		if entry == nil || entry.auth == nil {
			summary.total++
			continue
		}
		switch entry.state {
		case scheduledStateReady:
			summary.add(entry.auth, m.modelKey, false, blockReasonNone, time.Time{})
		case scheduledStateCooldown:
			summary.add(entry.auth, m.modelKey, true, blockReasonCooldown, entry.nextRetryAt)
		case scheduledStateDisabled:
			summary.add(entry.auth, m.modelKey, true, blockReasonDisabled, time.Time{})
		default:
			summary.add(entry.auth, m.modelKey, true, blockReasonOther, entry.nextRetryAt)
		}
	}
	return summary
}

// devyreLimitOverSignIn is what a request whose every try failed ends with, in place of its last
// error, when that error refused a sign-in (a 401, an invalid grant) while every account is held
// for a limit or signed out: the limit, as the router refuses such a request it tries nobody for
// (`errPick` is then its model cooldown), else from the accounts it tried, now held. Upstream ends
// it with the last error, so a request that met a signed-out account last, as every request does
// once that account's 30-minute retry comes up while the rest are held, ended as a refused
// sign-in: Claude Code said it could not authenticate, and a session waiting for usage stopped
// waiting. Nil leaves the last error as it is: a limit's own refusal (it carries the provider's
// reset), or any other failure. Not for Home's rounds, which time their own retries.
func (m *Manager) devyreLimitOverSignIn(homeMode bool, last, errPick error, tried map[string]struct{}, routeModel string) error {
	if homeMode || last == nil || !(isUnauthorizedError(last) || isInvalidGrantErrorMessage(last.Error())) {
		return nil
	}
	var cooldown *modelCooldownError
	if errors.As(errPick, &cooldown) && cooldown != nil {
		return errPick
	}
	if len(tried) == 0 {
		return nil
	}
	m.mu.RLock()
	auths := make([]*Auth, 0, len(tried))
	for id := range tried {
		if auth := m.auths[id]; auth != nil {
			auths = append(auths, auth)
		}
	}
	m.mu.RUnlock()
	now := time.Now()
	limit := m.devyreLimitOf(auths, routeModel, now)
	if !limit.refusedAsLimit() {
		return nil
	}
	return newModelCooldownErrorWithCause(routeModel, "", limit.firstHoldEnds.Sub(now), last)
}

// devyreSignedOut reports whether the provider refused the sign-in of the credential, for it or
// for model: its last error, or the model's, is a 401 or an invalid grant.
func devyreSignedOut(auth *Auth, model string) bool {
	if auth == nil {
		return false
	}
	if devyreSignInRefused(auth.LastError) {
		return true
	}
	key := canonicalModelKey(model)
	if key == "" {
		return false
	}
	for stateModel, state := range auth.ModelStates {
		if state != nil && canonicalModelKey(stateModel) == key && devyreSignInRefused(state.LastError) {
			return true
		}
	}
	return false
}

func devyreSignInRefused(err *Error) bool {
	return err != nil && (err.StatusCode() == http.StatusUnauthorized ||
		strings.EqualFold(strings.TrimSpace(err.Code), "unauthorized") || isInvalidGrantResultError(err))
}
