package auth

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
)

// devyre: exported views of the selector rules for the management
// quota-readings endpoint, so it ranks credentials exactly like the selectors.

// AuthPriority returns the priority tier the selectors use for auth: its
// integer "priority" attribute, or 0 when the attribute is missing or invalid.
// Higher tiers are tried first.
func AuthPriority(auth *Auth) int {
	return authPriority(auth)
}

// AuthSelectable reports whether the selectors would consider auth for model
// at now: it is not disabled, cooling down, rejected as unauthorized or
// otherwise unavailable. An empty model checks only credential-wide state.
func AuthSelectable(auth *Auth, model string, now time.Time) bool {
	blocked, _, _ := isAuthBlockedForModel(auth, model, now)
	return !blocked
}

// TransientlyAvoidedUntil reports until when the expiring-first selector places new
// sessions on other credentials than authID because authID failed transiently a
// moment ago, on any model (devyre_transient_soft.go), and false when it does not.
func TransientlyAvoidedUntil(authID string, now time.Time) (time.Time, bool) {
	return devyreTransientsDefault.avoidedUntil(authID, "", now)
}

// ExpiringFirstView is how the expiring-first selector sees one credential for
// one model: its effective quota reading, its gates and urgency, and the
// headroom of its windows.
type ExpiringFirstView struct {
	Reading    quotareading.Reading
	Evaluation quotareading.Evaluation
	Headroom   quotareading.Headroom
}

// ViewExpiringFirst evaluates auth for model at now the way the expiring-first
// selector does, reading its quota from store merged with its response-header
// signals. The empty model ignores model-scoped windows.
func ViewExpiringFirst(auth *Auth, model string, store *quotareading.Store, now time.Time, thresholds quotareading.Thresholds) ExpiringFirstView {
	return viewExpiringFirst(auth, model, store, now, thresholds)
}

// HasHeadroom reports whether the credential is placed before the ones without
// headroom: it is usable and its windows have headroom.
func (v ExpiringFirstView) HasHeadroom() bool {
	return v.Evaluation.Usable && v.Headroom.Has
}

// HeadroomReason says why the credential has no headroom: the windows at or
// below their thresholds, else its gates. It is empty with headroom.
func (v ExpiringFirstView) HeadroomReason() string {
	switch {
	case v.HasHeadroom():
		return ""
	case v.Headroom.Reason != "":
		return v.Headroom.Reason
	default:
		return v.Evaluation.GateReason
	}
}

// CompareExpiringFirstPlacement orders two credentials of one priority tier the
// way the expiring-first selector places a new session: negative when a comes
// first, positive when b does, and zero when they tie and the selector takes
// turns between them.
func CompareExpiringFirstPlacement(a, b ExpiringFirstView) int {
	return compareExpiringFirst(
		expiringFirstCandidate{eval: a.Evaluation, headroom: a.Headroom},
		expiringFirstCandidate{eval: b.Evaluation, headroom: b.Headroom},
		false,
	)
}
