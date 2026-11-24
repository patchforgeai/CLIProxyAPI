package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The credentials a refusal is decided over, as 2026-11-24 had them on one computer: one signed
// out (a 401, no refresh to come), one turned off, and one held for the model's own weekly limit
// until next.
func devyreRefusalCandidates(model string, next time.Time) (signedOut, disabled, held *Auth) {
	signedOut = &Auth{
		ID:            "devyre-refusal-signed-out",
		Provider:      "claude",
		Status:        StatusError,
		Unavailable:   true,
		StatusMessage: "unauthorized",
		LastError:     &Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
	}
	disabled = &Auth{ID: "devyre-refusal-disabled", Provider: "claude", Disabled: true, Status: StatusDisabled}
	held = &Auth{
		ID:       "devyre-refusal-held",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
				LastError:      &Error{Code: "rate_limit_error", Message: "rate limited", HTTPStatus: http.StatusTooManyRequests},
			},
		},
	}
	return signedOut, disabled, held
}

// Credentials signed out while in use, as the router marks them: the provider's 401 or an invalid
// grant, which it tries again 30 minutes later (MarkResult). They serve nothing until signed in
// again.
func devyreRefusalRevoked(model string, retry time.Time) (unauthorized, invalidGrant *Auth) {
	unauthorized = &Auth{
		ID:             "devyre-refusal-revoked",
		Provider:       "claude",
		Status:         StatusError,
		StatusMessage:  "unauthorized",
		Unavailable:    true,
		NextRetryAfter: retry,
		LastError:      &Error{Code: "unauthorized", Message: "invalid x-api-key", HTTPStatus: http.StatusUnauthorized},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: retry,
				LastError:      &Error{Message: "invalid x-api-key", HTTPStatus: http.StatusUnauthorized},
			},
		},
	}
	invalidGrant = &Auth{
		ID:       "devyre-refusal-invalid-grant",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: retry,
				LastError:      &Error{Code: "invalid_grant", Message: "invalid_grant: refresh token revoked", HTTPStatus: http.StatusBadRequest},
			},
		},
	}
	return unauthorized, invalidGrant
}

// A credential whose last request failed for a moment and that is tried again at next.
func devyreRefusalTransient(model string, next time.Time) *Auth {
	return &Auth{
		ID:       "devyre-refusal-transient",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				LastError:      &Error{Code: "server_error", Message: "overloaded", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
}

// devyreWantLimit checks that err is the model cooldown, ending when the held credential's hold
// ends as seen from before, a time taken before the refusal was asked for.
func devyreWantLimit(t *testing.T, err error, until, before time.Time) {
	t.Helper()
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("refusal = %T (%v), want the model cooldown", err, err)
	}
	if cooldown.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", cooldown.StatusCode())
	}
	if want := until.Sub(before); cooldown.resetIn > want || cooldown.resetIn < want-time.Minute {
		t.Fatalf("reset in %s, want the held credential's %s", cooldown.resetIn, want)
	}
}

func devyreWantUnavailable(t *testing.T, err error) {
	t.Helper()
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) {
		t.Fatalf("refusal = the model cooldown, want 503 no auth available")
	}
	var unavailable *authUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("refusal = %T (%v), want no auth available", err, err)
	}
	if status := unavailable.base.HTTPStatus; status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}

func TestDevyreLimitRefusal_SelectionBesideSignedOutAndDisabledIsTheLimit(t *testing.T) {
	t.Parallel()
	const model = "claude-fable-5-2"
	next := time.Now().Add(23 * time.Minute)
	signedOut, disabled, held := devyreRefusalCandidates(model, next)

	manager := NewManager(nil, nil, nil)
	before := time.Now()
	_, err := manager.availableAuthsForRouteModel([]*Auth{signedOut, held, disabled}, "claude", model, time.Now())
	devyreWantLimit(t, err, next, before)
}

func TestDevyreLimitRefusal_SelectionBesideATransientFailureStaysUnavailable(t *testing.T) {
	t.Parallel()
	const model = "claude-fable-5-2"
	next := time.Now().Add(23 * time.Minute)
	signedOut, _, held := devyreRefusalCandidates(model, next)
	transient := devyreRefusalTransient(model, time.Now().Add(30*time.Second))

	manager := NewManager(nil, nil, nil)
	_, err := manager.availableAuthsForRouteModel([]*Auth{signedOut, held, transient}, "claude", model, time.Now())
	devyreWantUnavailable(t, err)
}

func TestDevyreLimitRefusal_SelectionWithEveryCredentialSignedOutStaysTerminal(t *testing.T) {
	t.Parallel()
	const model = "claude-fable-5-2"
	signedOut, _, _ := devyreRefusalCandidates(model, time.Now().Add(time.Hour))

	manager := NewManager(nil, nil, nil)
	_, err := manager.availableAuthsForRouteModel([]*Auth{signedOut}, "claude", model, time.Now())
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) {
		t.Fatal("refusal = the model cooldown, want the terminal no auth available")
	}
	if !IsTerminalAuthError(err) {
		t.Fatalf("refusal = %T (%v), want a terminal auth error", err, err)
	}
}

func TestDevyreLimitRefusal_SchedulerBesideSignedOutIsTheLimit(t *testing.T) {
	const model = "devyre-limit-refusal-single"
	next := time.Now().Add(23 * time.Minute)
	signedOut, _, held := devyreRefusalCandidates(model, next)
	registerSchedulerModels(t, "claude", model, signedOut.ID, held.ID)

	scheduler := newSchedulerForTest(&RoundRobinSelector{}, signedOut, held)
	before := time.Now()
	_, err := scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
	devyreWantLimit(t, err, next, before)
}

func TestDevyreLimitRefusal_SchedulerBesideATransientFailureStaysUnavailable(t *testing.T) {
	const model = "devyre-limit-refusal-single-transient"
	next := time.Now().Add(23 * time.Minute)
	signedOut, _, held := devyreRefusalCandidates(model, next)
	transient := devyreRefusalTransient(model, time.Now().Add(30*time.Second))
	registerSchedulerModels(t, "claude", model, signedOut.ID, held.ID, transient.ID)

	scheduler := newSchedulerForTest(&RoundRobinSelector{}, signedOut, held, transient)
	_, err := scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
	devyreWantUnavailable(t, err)
}

func TestDevyreLimitRefusal_MixedSchedulerBesideSignedOutIsTheLimit(t *testing.T) {
	const model = "devyre-limit-refusal-mixed"
	next := time.Now().Add(23 * time.Minute)
	signedOut, _, held := devyreRefusalCandidates(model, next)
	registerSchedulerModels(t, "claude", model, signedOut.ID, held.ID)

	scheduler := newSchedulerForTest(&RoundRobinSelector{}, signedOut, held)
	before := time.Now()
	_, _, err := scheduler.pickMixed(context.Background(), []string{"claude"}, model, cliproxyexecutor.Options{}, nil)
	devyreWantLimit(t, err, next, before)
}

func TestDevyreLimitRefusal_SignedOutWhileInUseIsOutBesideALimit(t *testing.T) {
	const model = "devyre-limit-refusal-revoked"
	next := time.Now().Add(23 * time.Minute)
	retry := time.Now().Add(5 * time.Minute)
	_, _, held := devyreRefusalCandidates(model, next)
	unauthorized, invalidGrant := devyreRefusalRevoked(model, retry)
	registerSchedulerModels(t, "claude", model, held.ID, unauthorized.ID, invalidGrant.ID)
	auths := []*Auth{unauthorized, held, invalidGrant}

	// Through each way of picking, the limit, ending as its hold does: never at the signed-out
	// credentials' next try, which only meets the same refusal.
	manager := NewManager(nil, nil, nil)
	before := time.Now()
	_, err := manager.availableAuthsForRouteModel(auths, "claude", model, time.Now())
	devyreWantLimit(t, err, next, before)

	scheduler := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	before = time.Now()
	_, err = scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
	devyreWantLimit(t, err, next, before)
	before = time.Now()
	_, _, err = scheduler.pickMixed(context.Background(), []string{"claude"}, model, cliproxyexecutor.Options{}, nil)
	devyreWantLimit(t, err, next, before)
}

func TestDevyreLimitRefusal_SignedOutWhileInUseAloneIsNoLimit(t *testing.T) {
	const model = "devyre-limit-refusal-revoked-alone"
	retry := time.Now().Add(5 * time.Minute)
	unauthorized, invalidGrant := devyreRefusalRevoked(model, retry)
	registerSchedulerModels(t, "claude", model, unauthorized.ID, invalidGrant.ID)
	manager := NewManager(nil, nil, nil)
	_, err := manager.availableAuthsForRouteModel([]*Auth{unauthorized, invalidGrant}, "claude", model, time.Now())
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) {
		t.Fatalf("refusal = the model cooldown with no credential at a limit, want no auth available")
	}
}

func TestDevyreLimitOverSignIn(t *testing.T) {
	const model = "devyre-limit-over-sign-in"
	next := time.Now().Add(23 * time.Minute)
	_, _, held := devyreRefusalCandidates(model, next)
	unauthorized, _ := devyreRefusalRevoked(model, time.Now().Add(5*time.Minute))
	manager := NewManager(nil, nil, nil)
	manager.auths[held.ID] = held
	manager.auths[unauthorized.ID] = unauthorized
	signInRefused := &Error{Message: "invalid x-api-key", HTTPStatus: http.StatusUnauthorized}
	limitRefused := &Error{Message: "rate limited", HTTPStatus: http.StatusTooManyRequests}
	cooling := newModelCooldownError(model, "", time.Minute)
	noneLeft := &Error{Code: "auth_not_found", Message: "no auth candidates"}
	bothTried := map[string]struct{}{held.ID: {}, unauthorized.ID: {}}
	signedOutTried := map[string]struct{}{unauthorized.ID: {}}

	// The signed-out account's try came up, the rest are held: the router's own refusal.
	if got := manager.devyreLimitOverSignIn(false, signInRefused, cooling, signedOutTried, model); got != cooling {
		t.Fatalf("with the rest held = %v, want the router's model cooldown", got)
	}
	// Every account tried, the signed-out one last: the limit, from the accounts as they are held.
	before := time.Now()
	devyreWantLimit(t, manager.devyreLimitOverSignIn(false, signInRefused, noneLeft, bothTried, model), next, before)
	// The limit's own refusal last, carrying the provider's reset, stands.
	if got := manager.devyreLimitOverSignIn(false, limitRefused, noneLeft, bothTried, model); got != nil {
		t.Fatalf("with a limit's refusal last = %v, want it to stand", got)
	}
	// Signed out alone is no limit; Home's rounds time their own retries.
	if got := manager.devyreLimitOverSignIn(false, signInRefused, noneLeft, signedOutTried, model); got != nil {
		t.Fatalf("with only a signed-out account = %v, want the refusal to stand", got)
	}
	if got := manager.devyreLimitOverSignIn(true, signInRefused, cooling, signedOutTried, model); got != nil {
		t.Fatalf("in Home's rounds = %v, want nothing", got)
	}
}
