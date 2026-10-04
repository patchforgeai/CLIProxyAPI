package auth

import "time"

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
