package auth

// devyre: what a failed refresh does to the credential (devyre_sign_in.go; devyre/README.md, "A
// sign-in's life"). Upstream retried an invalid_grant every 1 to 30 minutes for good (Machine's
// log: 1,090 replays of four dead Claude tokens over nine days) and called every other failure
// with an expired access token "token expired", which the app read as signed out while the
// router tried again in 5 minutes.

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
	log "github.com/sirupsen/logrus"
)

const (
	// The next try after a refresh that never reached the server: seconds, growing to minutes.
	unreachableBackoffBase = 10 * time.Second
	unreachableBackoffMax  = 5 * time.Minute
	// The least a 429 delays the credential's next refresh (its Retry-After may say more).
	rateLimitedBackoffMin = time.Minute
	// maxUntypedAnswer bounds what is kept of the error of a refresh tokenrefresh did not send.
	maxUntypedAnswer = 200
)

// refreshOutcome is what a failed refresh means for the sign-in.
type refreshOutcome struct {
	kind   tokenrefresh.Kind
	answer string
	wait   time.Duration
}

// refreshOutcomeOf reads a refresh error: a tokenrefresh.Failure says it all; another provider's
// error is read by its text, an invalid grant or a 401 refusing the sign-in for good.
func refreshOutcomeOf(err error) refreshOutcome {
	if failure, ok := tokenrefresh.AsFailure(err); ok {
		return refreshOutcome{kind: failure.Kind, answer: failure.Answer(), wait: failure.Wait}
	}
	answer := safeErrorDiagnosticForLog(err)
	if len(answer) > maxUntypedAnswer {
		answer = answer[:maxUntypedAnswer] + "…"
	}
	switch {
	case isInvalidGrantError(err) || isUnauthorizedError(err):
		return refreshOutcome{kind: tokenrefresh.KindRefused, answer: answer}
	case statusCodeFromError(err) == http.StatusTooManyRequests:
		wait := time.Duration(0)
		if retryAfter := retryAfterFromError(err); retryAfter != nil {
			wait = *retryAfter
		}
		return refreshOutcome{kind: tokenrefresh.KindRateLimited, answer: answer, wait: wait}
	default:
		// Whether the request left is not known: it may have spent the token.
		return refreshOutcome{kind: tokenrefresh.KindLost, answer: answer}
	}
}

// passingRefreshBackoff is how long a credential waits for its next refresh after its
// failures-th passing failure in a row: seconds after one that never reached the server,
// growing to minutes; at least a minute, or what the provider asked, after a 429; 5 minutes after
// the rest.
func passingRefreshBackoff(outcome refreshOutcome, failures int) time.Duration {
	switch outcome.kind {
	case tokenrefresh.KindUnreachable:
		backoff := unreachableBackoffBase
		for i := 1; i < failures && backoff < unreachableBackoffMax; i++ {
			backoff *= 2
		}
		return min(backoff, unreachableBackoffMax)
	case tokenrefresh.KindRateLimited:
		return max(outcome.wait, rateLimitedBackoffMin)
	default:
		return refreshFailureBackoff
	}
}

// refreshFailure is what applyRefreshFailure did, for the caller to act on once m.mu is free.
type refreshFailure struct {
	reschedule bool
	unschedule bool
	// persist: the token file must say the sign-in ended.
	persist  *Auth
	from, to SignInStatus
	auth     *Auth
}

// applyRefreshFailure records a failed refresh on the credential current (under m.mu):
//
//   - The provider refused the refresh token for good: the sign-in ended. It is never refreshed
//     again until its tokens change, and the token file says so. An access token the provider
//     rejected, or that has run out, makes it unavailable at once; one that still runs keeps
//     serving until it does.
//   - A passing failure: the sign-in is retrying, tried again by passingRefreshBackoff, never
//     after the access token runs out while it runs. Without a usable access token the credential
//     is unavailable meanwhile.
//
// A disabled credential stays disabled either way.
func applyRefreshFailure(current *Auth, err error, failedAccessToken string, now time.Time) refreshFailure {
	outcome := refreshOutcomeOf(err)
	result := refreshFailure{from: SignInStatusOf(current), auth: current}
	current.Generation++
	current.UpdatedAt = now

	isDisabled := current.Disabled || current.Status == StatusDisabled
	hasValidAccessToken := current.HasValidAccessToken(now)
	// failedAccessToken is set only when upstream rejected this exact access token. Its expiry
	// time no longer proves it is usable.
	accessTokenRejected := failedAccessToken != "" && authAccessToken(current) == failedAccessToken

	if outcome.kind.Terminal() {
		markSignInEnded(current, outcome, now)
		current.NextRefreshAfter = time.Time{}
		current.RefreshFailures = 0
		// An access token that still runs keeps serving: what keeps the credential from serving
		// (LastError, its status) stays as it is until it runs out.
		switch {
		case isDisabled:
			current.Unavailable = true
			current.Status = StatusDisabled
			current.LastError = refreshErrorFromError(err)
			current.StatusMessage = "disabled (" + signInEndedMessage(outcome.kind) + ")"
		case accessTokenRejected || !hasValidAccessToken:
			// Neither token can recover without a new sign-in: stop selecting the credential
			// until its tokens change, instead of returning the same 401 to clients.
			current.Unavailable = true
			current.Status = StatusError
			current.NextRetryAfter = time.Time{}
			current.LastError = &Error{Code: "unauthorized", Message: err.Error(), HTTPStatus: http.StatusUnauthorized}
			current.StatusMessage = signInEndedMessage(outcome.kind)
		}
		result.unschedule = true
		result.persist = current.Clone()
		result.to = SignInStatusOf(current)
		return result
	}

	current.LastError = refreshErrorFromError(err)
	// Counted by the sign-in's own state, which the same tokens read again keep: the file's
	// first read after a start replaces the credential, RefreshFailures with it.
	failures, since := 1, now
	if retry := current.devyreSignIn; retry.active() {
		failures, since = retry.failures+1, retry.since
	}
	current.RefreshFailures = failures
	current.devyreSignIn = signInRetry{reason: outcome.kind, answer: outcome.answer, since: since, failures: failures}
	nextTry := now.Add(passingRefreshBackoff(outcome, failures))
	switch {
	case isDisabled:
		current.Unavailable = true
		current.Status = StatusDisabled
		if current.StatusMessage == "" {
			current.StatusMessage = "disabled"
		}
	case hasValidAccessToken:
		// It still serves: its status stays as it is, and its next try comes before it runs out.
		if exp, ok := current.AccessTokenExpirationTime(); ok && !exp.IsZero() && nextTry.After(exp) {
			nextTry = exp
		}
	default:
		current.Unavailable = true
		current.Status = StatusError
		current.StatusMessage = signInRetryingMessage(outcome.kind)
	}
	current.NextRefreshAfter = nextTry
	result.reschedule = true
	result.to = SignInStatusOf(current)
	return result
}

// logSignInChange says, at info, each change of how a credential's sign-in stands: into and out
// of retrying and ended, with the step that changed it, why, the provider's answer, and the
// next try.
func logSignInChange(auth *Auth, step string, from, to SignInStatus) {
	if auth == nil || (from.State == to.State && from.Reason == to.Reason) {
		return
	}
	fields := log.Fields{"credential": auth.ID, "provider": auth.Provider, "step": step, "from": signInStateWord(from.State), "to": signInStateWord(to.State)}
	switch to.State {
	case SignInEnded:
		fields["reason"] = string(to.Reason)
		fields["answer"] = to.Answer
		log.WithFields(fields).Infof("sign-in of %s ended: the provider refused its refresh token (%s), never refreshed again until it is signed in again", auth.ID, to.Answer)
	case SignInRetrying:
		fields["reason"] = string(to.Reason)
		fields["answer"] = to.Answer
		fields["next_try"] = to.NextTryAt.UTC().Format(time.RFC3339)
		log.WithFields(fields).Infof("sign-in of %s retrying: its refresh failed (%s: %s), next try at %s", auth.ID, to.Reason, to.Answer, to.NextTryAt.UTC().Format(time.RFC3339))
	default:
		if !from.Since.IsZero() {
			fields["for"] = time.Since(from.Since).Round(time.Second).String()
		}
		fields["reason"] = string(from.Reason)
		log.WithFields(fields).Infof("sign-in of %s refreshed again, after %s", auth.ID, signInStateWord(from.State))
	}
}

// logSignInEnd says, at info, when a refresh's answer moved (or first gave) the end of the
// sign-in.
func logSignInEnd(before, after *Auth) {
	was, hadEnd := SignInEndsAt(before)
	now, hasEnd := SignInEndsAt(after)
	if !hasEnd || (hadEnd && was.Equal(now)) {
		return
	}
	log.WithFields(log.Fields{"credential": after.ID, "provider": after.Provider, "ends_at": now.UTC().Format(time.RFC3339)}).
		Infof("sign-in of %s ends at %s, as its token answer says", after.ID, now.UTC().Format(time.RFC3339))
}

func signInStateWord(state SignInState) string {
	if state == SignInFine {
		return "fine"
	}
	return string(state)
}

// persistEndedSignIn writes that the sign-in ended to the token file, so a restart never sends
// the dead refresh token again.
func (m *Manager) persistEndedSignIn(ctx context.Context, auth *Auth) {
	if auth == nil {
		return
	}
	if errPersist := m.persist(ctx, auth); errPersist != nil {
		log.WithFields(log.Fields{"credential": auth.ID, "provider": auth.Provider}).Warnf("could not write that the sign-in of %s ended: %v", auth.ID, errPersist)
	}
}
