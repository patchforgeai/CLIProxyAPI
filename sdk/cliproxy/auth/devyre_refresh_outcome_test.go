package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// signInTestExecutor answers each refresh with the next of its answers: an error, or new tokens
// (nil).
type signInTestExecutor struct {
	schedulerProviderTestExecutor
	mu      sync.Mutex
	answers []error
	calls   atomic.Int32
}

func (e *signInTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.calls.Add(1)
	e.mu.Lock()
	var err error
	if len(e.answers) > 0 {
		err = e.answers[0]
		e.answers = e.answers[1:]
	}
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	auth.Metadata["access_token"] = "fresh-access"
	auth.Metadata["refresh_token"] = "next-refresh"
	auth.Metadata["expired"] = time.Now().Add(8 * time.Hour).Format(time.RFC3339)
	return auth, nil
}

// savingStore keeps what was saved.
type savingStore struct {
	mu    sync.Mutex
	saved map[string]*Auth
}

func (s *savingStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *savingStore) Delete(context.Context, string) error  { return nil }
func (s *savingStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = make(map[string]*Auth)
	}
	s.saved[auth.ID] = auth.Clone()
	return auth.ID, nil
}
func (s *savingStore) last(id string) *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[id]
}

func signInTestManager(t *testing.T, accessExpiresIn time.Duration, answers ...error) (*Manager, *signInTestExecutor, *savingStore, *Auth) {
	t.Helper()
	store := &savingStore{}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &signInTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "claude"}, answers: answers}
	manager.RegisterExecutor(executor)
	auth := &Auth{
		ID:       "claude-user@example.com.json",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "user@example.com",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"expired":       time.Now().Add(accessExpiresIn).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	return manager, executor, store, auth
}

func currentAuth(t *testing.T, manager *Manager, id string) *Auth {
	t.Helper()
	auth, ok := manager.GetByID(id)
	if !ok {
		t.Fatalf("no credential %s", id)
	}
	return auth
}

var claudeTimeLimit = &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindEnded, Status: http.StatusBadRequest, Code: "invalid_grant", Description: "Refresh token expired"}

// Machine's four falls: Claude's "Refresh token expired" at a routine refresh. The sign-in ended:
// never sent again (upstream replayed it every 30 minutes for days), said in its status and in
// the token file, so a restart does not send it either.
func TestDevyreClaudesTimeLimitEndsTheSignInAtOnce(t *testing.T) {
	manager, executor, store, auth := signInTestManager(t, -time.Hour, claudeTimeLimit)
	manager.refreshAuth(context.Background(), auth.ID)

	got := currentAuth(t, manager, auth.ID)
	status := SignInStatusOf(got)
	if status.State != SignInEnded || status.Reason != tokenrefresh.KindEnded || status.Answer != "invalid_grant: Refresh token expired" {
		t.Fatalf("sign-in = %+v, want ended at its time limit", status)
	}
	if !hasUnauthorizedAuthFailure(got) || got.StatusMessage != "sign-in ended by the provider (time limit)" {
		t.Fatalf("an expired credential whose sign-in ended: unauthorized %t, status %q", hasUnauthorizedAuthFailure(got), got.StatusMessage)
	}
	if saved := store.last(auth.ID); saved == nil || !hasEndedSignIn(saved) {
		t.Fatal("the token file does not say the sign-in ended")
	}
	for _, at := range []time.Duration{time.Minute, 30 * time.Minute, 24 * time.Hour} {
		if manager.shouldRefresh(got, time.Now().Add(at)) {
			t.Fatalf("an ended sign-in is due again after %s", at)
		}
	}
	manager.refreshAuth(context.Background(), auth.ID)
	if calls := executor.calls.Load(); calls != 1 {
		t.Fatalf("the dead refresh token was sent %d times", calls)
	}

	// The router starting again reads it from the file and sends nothing.
	restarted := got.Clone()
	restarted.Unavailable, restarted.Status, restarted.StatusMessage, restarted.LastError = false, StatusActive, "", nil
	restarted.devyreSignIn = signInRetry{}
	if _, scheduled := nextRefreshCheckAt(time.Now(), restarted, time.Second); scheduled {
		t.Fatal("a restart schedules a refresh of a sign-in the file says ended")
	}
}

func TestDevyreAnEndedSignInServesWhileItsAccessTokenRuns(t *testing.T) {
	manager, executor, _, auth := signInTestManager(t, 3*time.Hour, &tokenrefresh.Failure{Provider: "codex", Kind: tokenrefresh.KindReused, Status: 401, Code: "refresh_token_reused"})
	manager.refreshAuth(context.Background(), auth.ID)
	got := currentAuth(t, manager, auth.ID)
	if status := SignInStatusOf(got); status.State != SignInEnded || status.Reason != tokenrefresh.KindReused {
		t.Fatalf("sign-in = %+v, want ended (reused)", status)
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "", time.Now()); blocked || got.Unavailable {
		t.Fatal("an access token with three hours left stopped serving")
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "", time.Now().Add(4*time.Hour)); !blocked {
		t.Fatal("it still serves once its access token ran out")
	}
	// A request's 401 sends nothing either.
	if _, refreshed := manager.tryRefreshAfterUnauthorized(context.Background(), got, errors.New("status 401: unauthorized"), false); refreshed || executor.calls.Load() != 1 {
		t.Fatal("a request's 401 refreshed an ended sign-in")
	}
}

// A passing failure is retrying, never "signed out": tried again by why it failed, the access
// token serving while it lasts.
func TestDevyreAPassingFailureRetriesByWhyItFailed(t *testing.T) {
	unreachable := &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindUnreachable, Err: errors.New("dial tcp: lookup platform.claude.com: no such host")}
	lost := &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindLost, Err: errors.New("read: connection reset by peer")}
	limited := &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindRateLimited, Status: 429, Wait: 2 * time.Minute}
	manager, _, _, auth := signInTestManager(t, -time.Hour, unreachable, unreachable, unreachable, lost, limited)

	wants := []struct {
		reason tokenrefresh.Kind
		wait   time.Duration
	}{
		{tokenrefresh.KindUnreachable, 10 * time.Second},
		{tokenrefresh.KindUnreachable, 20 * time.Second},
		{tokenrefresh.KindUnreachable, 40 * time.Second},
		{tokenrefresh.KindLost, refreshFailureBackoff},
		{tokenrefresh.KindRateLimited, 2 * time.Minute},
	}
	var since time.Time
	for i, want := range wants {
		before := time.Now()
		manager.refreshAuth(context.Background(), auth.ID)
		got := currentAuth(t, manager, auth.ID)
		status := SignInStatusOf(got)
		if status.State != SignInRetrying || status.Reason != want.reason {
			t.Fatalf("failure %d: sign-in = %+v, want retrying (%s)", i+1, status, want.reason)
		}
		if wait := got.NextRefreshAfter.Sub(before); wait < want.wait-time.Second || wait > want.wait+time.Second {
			t.Fatalf("failure %d: next try in %s, want %s", i+1, wait, want.wait)
		}
		if !got.Unavailable || got.StatusMessage != "refresh retrying ("+string(want.reason)+")" || hasUnauthorizedAuthFailure(got) {
			t.Fatalf("failure %d: unavailable %t, status %q, terminal %t", i+1, got.Unavailable, got.StatusMessage, hasUnauthorizedAuthFailure(got))
		}
		if i == 0 {
			since = status.Since
		} else if !status.Since.Equal(since) {
			t.Fatalf("failure %d: retrying since %s, want since the first failure %s", i+1, status.Since, since)
		}
		// Due at the next try, as the loop sees it.
		got.NextRefreshAfter = time.Now().Add(-time.Second)
		manager.mu.Lock()
		manager.auths[auth.ID].NextRefreshAfter = got.NextRefreshAfter
		manager.mu.Unlock()
	}

	// Back: fine again, nothing left of the failures.
	manager.refreshAuth(context.Background(), auth.ID)
	got := currentAuth(t, manager, auth.ID)
	if status := SignInStatusOf(got); status.State != SignInFine || got.Unavailable || got.StatusMessage != "" {
		t.Fatalf("after a refresh that worked: sign-in %+v, unavailable %t, status %q", status, got.Unavailable, got.StatusMessage)
	}
}

func TestDevyreARateLimitWaitsAtLeastAMinute(t *testing.T) {
	if got := passingRefreshBackoff(refreshOutcome{kind: tokenrefresh.KindRateLimited, wait: 5 * time.Second}, 1); got != time.Minute {
		t.Fatalf("a 5 s Retry-After waits %s, want a minute", got)
	}
	if got := passingRefreshBackoff(refreshOutcome{kind: tokenrefresh.KindUnreachable}, 30); got != unreachableBackoffMax {
		t.Fatalf("the 30th unreachable waits %s, want %s", got, unreachableBackoffMax)
	}
}

func TestDevyreAPassingFailureWithAWorkingAccessTokenServesOn(t *testing.T) {
	manager, _, _, auth := signInTestManager(t, 2*time.Minute, &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindServer, Status: 503})
	manager.refreshAuth(context.Background(), auth.ID)
	got := currentAuth(t, manager, auth.ID)
	if status := SignInStatusOf(got); status.State != SignInRetrying {
		t.Fatalf("sign-in = %+v, want retrying", status)
	}
	if got.Unavailable || got.Status == StatusError {
		t.Fatal("a working access token stopped serving after a passing failure")
	}
	expiry, _ := got.AccessTokenExpirationTime()
	if got.NextRefreshAfter.After(expiry) {
		t.Fatalf("next try %s comes after the access token runs out at %s", got.NextRefreshAfter, expiry)
	}
}

// The same tokens read again (the watcher, after the router's own write) keep what their
// refreshes said; new ones (a new sign-in) are a new chance.
func TestDevyreANewSignInIsANewChanceTheSameTokensAreNot(t *testing.T) {
	manager, executor, _, auth := signInTestManager(t, -time.Hour, claudeTimeLimit)
	manager.refreshAuth(context.Background(), auth.ID)
	ended := currentAuth(t, manager, auth.ID)

	echo := &Auth{ID: auth.ID, Provider: "claude", Status: StatusActive, Metadata: map[string]any{}}
	for k, v := range ended.Metadata {
		echo.Metadata[k] = v
	}
	if _, err := manager.Update(context.Background(), echo); err != nil {
		t.Fatal(err)
	}
	got := currentAuth(t, manager, auth.ID)
	if !hasUnauthorizedAuthFailure(got) || !hasEndedSignIn(got) {
		t.Fatalf("the file read again revived an ended sign-in: status %q, unavailable %t", got.StatusMessage, got.Unavailable)
	}

	fresh := &Auth{ID: auth.ID, Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"type": "claude", "email": "user@example.com", "access_token": "new-sign-in-access", "refresh_token": "new-sign-in-refresh",
		"expired": time.Now().Add(8 * time.Hour).Format(time.RFC3339),
	}}
	if _, err := manager.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	got = currentAuth(t, manager, auth.ID)
	if status := SignInStatusOf(got); status.State != SignInFine || got.Unavailable {
		t.Fatalf("a new sign-in: sign-in %+v, unavailable %t, want fine", status, got.Unavailable)
	}
	manager.refreshAuth(context.Background(), auth.ID)
	if calls := executor.calls.Load(); calls != 2 {
		t.Fatalf("refreshes sent = %d, want the new sign-in's refreshed", calls)
	}
}

func TestDevyreEveryChangeOfASignInIsLogged(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	previous := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	defer log.SetLevel(previous)

	unreachable := &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindUnreachable, Err: errors.New("dial: no route to host")}
	manager, _, _, auth := signInTestManager(t, -time.Hour, unreachable, nil, claudeTimeLimit)
	manager.refreshAuth(context.Background(), auth.ID)
	manager.mu.Lock()
	manager.auths[auth.ID].NextRefreshAfter = time.Time{}
	manager.mu.Unlock()
	manager.refreshAuth(context.Background(), auth.ID)
	manager.mu.Lock()
	manager.auths[auth.ID].Metadata["expired"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	manager.mu.Unlock()
	manager.refreshAuth(context.Background(), auth.ID)

	var changes []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.InfoLevel && strings.HasPrefix(entry.Message, "sign-in of "+auth.ID) {
			if entry.Data["credential"] != auth.ID || entry.Data["step"] != "refresh" {
				t.Fatalf("a change logged without its credential or step: %+v", entry.Data)
			}
			changes = append(changes, entry.Data["from"].(string)+">"+entry.Data["to"].(string))
		}
	}
	if strings.Join(changes, " ") != "fine>retrying retrying>fine fine>ended" {
		t.Fatalf("logged changes %v", changes)
	}
}

func TestDevyreAWakeHoldsDueRefreshesForTheNetwork(t *testing.T) {
	loop := newAuthAutoRefreshLoop(NewManager(nil, nil, nil), time.Second, 1)
	start := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	loop.timerArmed = true
	loop.notePass(start)
	loop.notePass(start.Add(30 * time.Second))
	if !loop.wake.until.IsZero() {
		t.Fatal("a pass on time reads as a wake")
	}
	woke := start.Add(9 * time.Hour)
	loop.notePass(woke)
	if !loop.wake.until.Equal(woke.Add(wakeSettle)) {
		t.Fatalf("settle until %s, want %s", loop.wake.until, woke.Add(wakeSettle))
	}
	if !loop.waitForNetwork([]string{"a.json"}, woke) {
		t.Fatal("due refreshes went out right after a wake")
	}
	if next, ok := loop.peek(); !ok || !next.Equal(woke.Add(wakeSettle)) {
		t.Fatalf("the due refresh waits until %s, want %s", next, woke.Add(wakeSettle))
	}
	if loop.waitForNetwork([]string{"a.json"}, woke.Add(wakeSettle)) {
		t.Fatal("due refreshes still wait once the network had its time")
	}

	// With nothing scheduled the timer slept on its own: no wake.
	idle := newAuthAutoRefreshLoop(NewManager(nil, nil, nil), time.Second, 1)
	idle.notePass(start)
	idle.notePass(start.Add(9 * time.Hour))
	if !idle.wake.until.IsZero() {
		t.Fatal("an idle loop read its own sleep as a wake")
	}
}

// A refresh given up before it was sent (its caller's deadline, while it waited for a slot) says
// nothing of the sign-in: no state, no backoff.
func TestDevyreARefreshNotSentChangesNothing(t *testing.T) {
	notSent := fmt.Errorf("%w: %w", tokenrefresh.ErrNotSent, context.DeadlineExceeded)
	manager, _, _, auth := signInTestManager(t, -time.Hour, notSent)
	manager.refreshAuth(context.Background(), auth.ID)
	got := currentAuth(t, manager, auth.ID)
	if status := SignInStatusOf(got); status.State != SignInFine || got.StatusMessage != "" {
		t.Fatalf("sign-in = %+v, status %q, want untouched", status, got.StatusMessage)
	}
}

// The backoff of a request that never left grows from the sign-in's own count, which the same
// tokens read again (the start's first full load) do not reset.
func TestDevyreTheBackoffSurvivesTheFileReadAgain(t *testing.T) {
	unreachable := &tokenrefresh.Failure{Provider: "claude", Kind: tokenrefresh.KindUnreachable, Err: errors.New("dial: no route to host")}
	manager, _, _, auth := signInTestManager(t, -time.Hour, unreachable, unreachable)
	manager.refreshAuth(context.Background(), auth.ID)
	echo := currentAuth(t, manager, auth.ID)
	fresh := &Auth{ID: auth.ID, Provider: "claude", Status: StatusActive, Metadata: map[string]any{}}
	for key, value := range echo.Metadata {
		fresh.Metadata[key] = value
	}
	if _, err := manager.Update(WithSkipPersist(context.Background()), fresh); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.auths[auth.ID].NextRefreshAfter = time.Time{}
	manager.mu.Unlock()
	before := time.Now()
	manager.refreshAuth(context.Background(), auth.ID)
	got := currentAuth(t, manager, auth.ID)
	if wait := got.NextRefreshAfter.Sub(before); wait < 19*time.Second || wait > 21*time.Second {
		t.Fatalf("second failure waits %s, want 20 s (the count kept across the file read again)", wait)
	}
}

// After a wake, the wait ends as soon as the network answers: the refreshes it held go out then,
// not 20 s later.
func TestDevyreTheWakeWaitEndsWhenTheNetworkAnswers(t *testing.T) {
	every := wakeProbeEvery
	wakeProbeEvery = 5 * time.Millisecond
	t.Cleanup(func() { wakeProbeEvery = every })
	manager := NewManager(nil, nil, nil)
	var looks atomic.Int32
	manager.SetNetworkProbe(func(context.Context) bool { return looks.Add(1) >= 3 })
	loop := newAuthAutoRefreshLoop(manager, time.Second, 1)
	loop.timerArmed = true
	now := time.Now()
	loop.notePass(now.Add(-9 * time.Hour))
	loop.notePass(now)
	if !loop.waitForNetwork([]string{"a.json"}, now) {
		t.Fatal("due refreshes went out right after a wake")
	}
	// Once it answers, the refresh the wait held is due again (rescheduled), and the wait is over.
	due := func() bool {
		loop.mu.Lock()
		defer loop.mu.Unlock()
		_, dirty := loop.dirty["a.json"]
		return dirty
	}
	deadline := time.Now().Add(2 * time.Second)
	for !due() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !due() {
		t.Fatal("the refresh the wait held is not due again")
	}
	if loop.waitForNetwork([]string{"a.json"}, time.Now()) {
		t.Fatal("the wait did not end once the network answered")
	}
	if got := looks.Load(); got != 3 {
		t.Fatalf("the network was looked at %d times, want until it answered (3)", got)
	}
}
