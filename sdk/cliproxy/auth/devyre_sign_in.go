package auth

// devyre: a sign-in's life (devyre/README.md, "A sign-in's life").
//
// Claude ends a sign-in about four weeks after it was made, at a time its token answer gives
// (refresh_token_expires_in), and refreshing never moves that time later. The credential keeps
// that end in its token file, the earlier of the kept and the new one at each refresh, and a new
// sign-in replaces it: the key is a token payload key (IsAuthTokenPayloadKey), which a new
// sign-in never takes from the file it replaces. A sign-in made before ends were kept has none
// until an answer gives one; nothing infers it (a file's times survive a new sign-in on Windows
// and macOS).
//
// A refresh the provider refused for good (tokenrefresh.Kind.Terminal: Claude's "Refresh token
// expired", OpenAI's refresh_token_expired, _reused and _invalidated, any other invalid_grant or
// 401) ends the sign-in: the credential is never refreshed again until its tokens change (a new
// sign-in, or another writer of its file), the token file keeps that it ended (so a restart does
// not send the dead token again), and an access token that has not run out keeps serving until
// it does. A refresh that failed in passing (the network, a timeout, a 429, a 5xx, a Cloudflare
// page) is retrying: tried again at a time that depends on why, its access token serving while
// it lasts.

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
)

const (
	// MetadataSignInEndsAt is the token file's key for when the sign-in ends (RFC 3339).
	MetadataSignInEndsAt = "refresh_token_expires_at"
	// MetadataSignInEnded is the token file's key for a sign-in its provider ended: an object
	// with why (`reason`, a tokenrefresh.Kind), the provider's answer (`answer`) and when (`at`).
	MetadataSignInEnded = "sign_in_ended"
)

// SignInEndsAt is when the credential's sign-in ends, and whether an answer said.
func SignInEndsAt(auth *Auth) (time.Time, bool) {
	return tokenrefresh.ParseEnd(authMetadataString(auth, MetadataSignInEndsAt))
}

// NoteSignInEnd keeps the end a refresh answered with (RFC 3339, empty when the answer gave
// none) by tokenrefresh.EndAfterRefresh: the earlier of the kept one and it.
func NoteSignInEnd(auth *Auth, answered string) {
	if auth == nil {
		return
	}
	end := tokenrefresh.EndAfterRefresh(authMetadataString(auth, MetadataSignInEndsAt), answered)
	if end == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[MetadataSignInEndsAt] = end
}

// SignInState is how a credential's sign-in stands, by its refreshes.
type SignInState string

const (
	// SignInFine: the last refresh worked, or none has failed.
	SignInFine SignInState = ""
	// SignInRetrying: the last refresh failed in passing; it is tried again at NextTryAt.
	SignInRetrying SignInState = "retrying"
	// SignInEnded: the provider ended the sign-in; only new tokens bring it back.
	SignInEnded SignInState = "ended"
)

// SignInStatus is what a credential's refreshes said of its sign-in.
type SignInStatus struct {
	State SignInState
	// Reason is why (the failure's tokenrefresh.Kind); empty while fine.
	Reason tokenrefresh.Kind
	// Answer is the provider's answer in a few words; never a token.
	Answer string
	// Since is when the state began.
	Since time.Time
	// NextTryAt is when a retrying sign-in is refreshed again.
	NextTryAt time.Time
}

// signInRetry is a credential's passing refresh failure, in memory (Auth.devyreSignIn).
type signInRetry struct {
	reason   tokenrefresh.Kind
	answer   string
	since    time.Time
	failures int
}

func (r signInRetry) active() bool { return r.reason != "" }

// SignInStatusOf is how auth's sign-in stands.
func SignInStatusOf(auth *Auth) SignInStatus {
	if auth == nil {
		return SignInStatus{}
	}
	if ended, ok := signInEnded(auth); ok {
		return ended
	}
	if retry := auth.devyreSignIn; retry.active() {
		return SignInStatus{State: SignInRetrying, Reason: retry.reason, Answer: retry.answer, Since: retry.since, NextTryAt: auth.NextRefreshAfter}
	}
	return SignInStatus{}
}

// signInEnded reads the ended sign-in the token file keeps.
func signInEnded(auth *Auth) (SignInStatus, bool) {
	if auth == nil || auth.Metadata == nil {
		return SignInStatus{}, false
	}
	raw, ok := auth.Metadata[MetadataSignInEnded].(map[string]any)
	if !ok {
		return SignInStatus{}, false
	}
	text := func(key string) string {
		value, _ := raw[key].(string)
		return value
	}
	reason := tokenrefresh.Kind(text("reason"))
	if !reason.Terminal() {
		reason = tokenrefresh.KindRefused
	}
	since, _ := tokenrefresh.ParseEnd(text("at"))
	return SignInStatus{State: SignInEnded, Reason: reason, Answer: text("answer"), Since: since}, true
}

// hasEndedSignIn reports whether the provider ended auth's sign-in: never refreshed again.
func hasEndedSignIn(auth *Auth) bool {
	_, ended := signInEnded(auth)
	return ended
}

func markSignInEnded(auth *Auth, outcome refreshOutcome, now time.Time) {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[MetadataSignInEnded] = map[string]any{
		"reason": string(outcome.kind),
		"answer": outcome.answer,
		"at":     tokenrefresh.FormatEnd(now),
	}
	auth.devyreSignIn = signInRetry{}
}

// clearSignInState forgets what failed refreshes said: the sign-in works again.
func clearSignInState(auth *Auth) {
	if auth == nil {
		return
	}
	delete(auth.Metadata, MetadataSignInEnded)
	auth.devyreSignIn = signInRetry{}
}

// signInEndedMessage is the status message of a sign-in its provider ended, by why.
func signInEndedMessage(kind tokenrefresh.Kind) string {
	switch kind {
	case tokenrefresh.KindEnded:
		return "sign-in ended by the provider (time limit)"
	case tokenrefresh.KindReused:
		return "sign-in ended by the provider (refresh token reused)"
	case tokenrefresh.KindRevoked:
		return "sign-in ended by the provider (revoked)"
	case tokenrefresh.KindOnHold:
		return "sign-in ended by the provider (account on hold)"
	default:
		return "sign-in ended by the provider (refresh token refused)"
	}
}

// signInRetryingMessage is the status message of a credential without a usable access token
// whose refresh failed in passing.
func signInRetryingMessage(kind tokenrefresh.Kind) string {
	return "refresh retrying (" + string(kind) + ")"
}

// devyreKeepSignInState carries what existing's refreshes said over to its replacement with the
// same tokens (a file the watcher read again, the router's own write of it): a passing failure,
// and a sign-in the provider refused for good, which the file alone does not say in full. New
// tokens (a new sign-in) are a new chance, and take none of it.
func devyreKeepSignInState(existing, replacement *Auth) {
	if existing == nil || replacement == nil {
		return
	}
	if !replacement.devyreSignIn.active() {
		replacement.devyreSignIn = existing.devyreSignIn
	}
	if hasUnauthorizedAuthFailure(existing) && !hasUnauthorizedAuthFailure(replacement) {
		replacement.Unavailable = existing.Unavailable
		replacement.Status = existing.Status
		replacement.StatusMessage = existing.StatusMessage
		replacement.LastError = cloneError(existing.LastError)
		replacement.NextRefreshAfter = existing.NextRefreshAfter
		replacement.NextRetryAfter = existing.NextRetryAfter
	}
}
