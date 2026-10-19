package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// efSessionOpts is a Claude Code request of session id.
func efSessionOpts(id string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{id}},
		Metadata: map[string]any{},
	}
}

// efFailoverContext carries a failover placement whose credentials have bindings.
func efFailoverContext(bindings map[string]int) (context.Context, *placement) {
	p := &placement{kind: placementFailover, bindings: func() map[string]int { return bindings }}
	return withPlacement(context.Background(), p), p
}

// efAffinityLogs returns the session-affinity lines hook captured.
func efAffinityLogs(hook interface{ AllEntries() []*log.Entry }) []string {
	var lines []string
	for _, entry := range hook.AllEntries() {
		if strings.HasPrefix(entry.Message, "session-affinity:") {
			lines = append(lines, entry.Message)
		}
	}
	return lines
}

func TestExpiringFirstFailoverRanksByStayPower(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		auths      []*Auth
		bindings   map[string]int
		wantNew    string // what a new placement picks
		wantID     string // what the failover picks
		wantReason string
	}{
		{
			// B is more urgent, but it already carries a session and less of its
			// 5h window is left per session: 80/2 = 40 against C's 60/1.
			name: "stay power outranks urgency",
			auths: []*Auth{
				efClaudeAuth("b", efWindow{"5h", 80, 3 * efHour}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("c", efWindow{"5h", 60, 3 * efHour}, efWindow{"7d", 90, 6 * efDay}),
			},
			bindings: map[string]int{"b": 1},
			wantNew:  "b", wantID: "c", wantReason: expiringFirstReasonFailoverStayPower,
		},
		{
			name: "equal stay power falls back to urgency",
			auths: []*Auth{
				efClaudeAuth("b", efWindow{"5h", 80, 3 * efHour}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("c", efWindow{"5h", 40, 3 * efHour}, efWindow{"7d", 90, 6 * efDay}),
			},
			bindings: map[string]int{"b": 1},
			wantNew:  "b", wantID: "b", wantReason: expiringFirstReasonFailoverStayPower,
		},
		{
			// Without a known short window a credential stays for 100 per session.
			name: "a credential without a short window counts 100",
			auths: []*Auth{
				efClaudeAuth("b", efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("c", efWindow{"7d", 90, 6 * efDay}),
			},
			wantNew: "b", wantID: "c", wantReason: expiringFirstReasonFailoverStayPower,
		},
		{
			name: "credentials without readings rank by stay power too",
			auths: []*Auth{
				efClaudeAuth("gated", efWindow{"5h", 1, efHour}, efWindow{"7d", 90, efHour}),
				efClaudeAuth("u1"),
				efClaudeAuth("u2"),
			},
			bindings: map[string]int{"u1": 2},
			wantNew:  "u1", wantID: "u2", wantReason: expiringFirstReasonFailoverStayPower,
		},
		{
			// The pools keep their order: known urgencies with headroom first.
			name: "the pools still come first",
			auths: []*Auth{
				efClaudeAuth("known", efWindow{"5h", 30, 3 * efHour}, efWindow{"7d", 90, 6 * efDay}),
				efClaudeAuth("unknown"),
			},
			wantNew: "known", wantID: "known", wantReason: expiringFirstReasonFailoverStayPower,
		},
		{
			name: "without headroom the most urgent still wins",
			auths: []*Auth{
				efClaudeAuth("l1", efWindow{"5h", 15, efHour}, efWindow{"7d", 50, 5 * efHour}),
				efClaudeAuth("l2", efWindow{"5h", 10, efHour}, efWindow{"7d", 40, 2 * efHour}),
			},
			bindings: map[string]int{"l2": 5},
			wantNew:  "l2", wantID: "l2", wantReason: expiringFirstReasonFailoverLowHeadroom,
		},
		{
			name: "every candidate gated",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 0, 2 * efHour}, efWindow{"7d", 90, 6 * efDay}),
				efClaudeAuth("b", efWindow{"5h", 1, 30 * time.Minute}, efWindow{"7d", 10, 6 * efDay}),
			},
			wantNew: "b", wantID: "b", wantReason: expiringFirstReasonFailoverAllGated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			placedNew, _, err := newHeadroomTestSelector().pick(context.Background(), "claude", "claude-opus-5-5", tc.auths)
			if err != nil || placedNew.auth.ID != tc.wantNew {
				t.Fatalf("new placement = %+v, %v; want %s", placedNew.auth, err, tc.wantNew)
			}
			ctx, place := efFailoverContext(tc.bindings)
			picked, reason, err := newHeadroomTestSelector().pick(ctx, "claude", "claude-opus-5-5", tc.auths)
			if err != nil || picked.auth.ID != tc.wantID || reason != tc.wantReason || place.reason != tc.wantReason {
				t.Fatalf("failover = %+v %q (placement %q), %v; want %s %q", picked.auth, reason, place.reason, err, tc.wantID, tc.wantReason)
			}
		})
	}
}

// A usable focus takes a failover too: stay power orders only the credentials
// outside a focus.
func TestExpiringFirstFailoverGoesToAUsableFocus(t *testing.T) {
	t.Parallel()

	// By stay power the failover would go to B (80/1 against F's 30/5).
	auths := []*Auth{
		efClaudeAuth("b", efWindow{"5h", 80, 3 * efHour}, efWindow{"7d", 50, efDay}),
		efClaudeAuth("f", efWindow{"5h", 30, 3 * efHour}, efWindow{"7d", 90, 6 * efDay}),
	}
	bindings := map[string]int{"f": 4}
	ctx, _ := efFailoverContext(bindings)
	if picked, _, err := newHeadroomTestSelector().pick(ctx, "claude", "claude-opus-5-5", auths); err != nil || picked.auth.ID != "b" {
		t.Fatalf("failover without a focus = %+v, %v; want b", picked.auth, err)
	}
	selector := newHeadroomTestSelector()
	selector.Focus = efFocusStore(t, auths[1])
	ctx, place := efFailoverContext(bindings)
	picked, reason, err := selector.pick(ctx, "claude", "claude-opus-5-5", auths)
	if err != nil || picked.auth.ID != "f" || reason != expiringFirstReasonFocus || place.reason != expiringFirstReasonFocus {
		t.Fatalf("failover with a focus = %+v %q (placement %q), %v; want f %q", picked.auth, reason, place.reason, err, expiringFirstReasonFocus)
	}
}

func TestExpiringFirstFailoverLogsStayPower(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	selector := newHeadroomTestSelector()
	selector.LogPicks = true
	ctx, _ := efFailoverContext(map[string]int{"auth-b": 1})
	auths := []*Auth{efClaudeAuth("auth-b", efWindow{"5h", 80, 3 * efHour}, efWindow{"7d", 50, efDay})}
	if _, err := selector.Pick(ctx, "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, auths); err != nil {
		t.Fatal(err)
	}
	entries := efPickEntries(hook)
	if len(entries) != 1 || entries[0].Data["reason"] != "failover-stay-power" ||
		entries[0].Message != "expiring-first: pick | auth=auth-b urgency=2.083 window=7d stay_power=40 bindings=1" {
		t.Fatalf("pick log entries = %+v", entries)
	}
}

// Sessions that leave one credential together spread over the credentials
// that can carry them, by headroom and load, instead of piling onto the most
// urgent one, which would then run out and send them all moving again.
func TestExpiringFirstFailoverSpreadsAHerd(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newHeadroomTestSelector(), TTL: time.Hour})
	defer selector.Stop()
	a := efClaudeAuth("auth-a", efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 40, 3 * efHour}) // 13.3%/h
	b := efClaudeAuth("auth-b", efWindow{"5h", 80, 3 * efHour}, efWindow{"7d", 50, efDay})      // 2.08%/h
	c := efClaudeAuth("auth-c", efWindow{"5h", 60, 3 * efHour}, efWindow{"7d", 90, 6 * efDay})  // 0.63%/h
	auths := []*Auth{a, b, c}
	pick := func(session string) string {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts(session), auths)
		if err != nil || got == nil {
			t.Fatalf("Pick(%s) = %+v, %v", session, got, err)
		}
		return got.ID
	}

	sessions := make([]string, 6)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("herd-%d", i)
		if got := pick(sessions[i]); got != "auth-a" {
			t.Fatalf("new session %d went to %s, want the most urgent auth-a", i, got)
		}
	}

	// A runs out. Each session fails over on its next request: stay power is
	// B 80/(1+n) against C 60/(1+n), so the herd splits B, C, B, C, B, and the
	// last tie at 20 goes to B, the more urgent.
	a.Unavailable = true
	hook.Reset()
	var moved []string
	for _, session := range sessions {
		moved = append(moved, pick(session))
	}
	if got := strings.Join(moved, ","); got != "auth-b,auth-c,auth-b,auth-c,auth-b,auth-b" {
		t.Fatalf("failovers went to %s", got)
	}
	lines := efAffinityLogs(hook)
	if len(lines) != len(sessions) {
		t.Fatalf("session-affinity lines = %q", lines)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "session-affinity: cache hit but auth unavailable, reselected | session=claude:herd-") ||
			!strings.Contains(line, " from=auth-a ") || !strings.HasSuffix(line, " reason=failover-stay-power") {
			t.Fatalf("failover line = %q", line)
		}
	}

	// The moved sessions stay where they went.
	a.Unavailable = false
	for i, session := range sessions {
		if got := pick(session); got != moved[i] {
			t.Fatalf("session %d moved again to %s, want %s", i, got, moved[i])
		}
	}
}

// A session made a request within the last 15 minutes counts as load; an
// older one does not.
func TestExpiringFirstFailoverCountsActiveBindingsOnly(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newHeadroomTestSelector(), TTL: time.Hour})
	defer selector.Stop()
	now := time.Now()
	// Two sessions on B: one used 10 minutes ago, one 20 minutes ago.
	selector.cache.mu.Lock()
	selector.cache.replaceAliasGroupsLocked("auth-b", now.Add(50*time.Minute), []string{"claude::claude:recent::claude-opus-5-5"})
	selector.cache.replaceAliasGroupsLocked("auth-b", now.Add(40*time.Minute), []string{"claude::claude:idle::claude-opus-5-5"})
	selector.cache.mu.Unlock()
	counts := selector.devyreFailover("auth-a", "claude", "claude-opus-5-5(high)").bindings()
	if len(counts) != 1 || counts["auth-b"] != 1 {
		t.Fatalf("active bindings = %v, want auth-b: 1", counts)
	}
	if p := selector.devyrePlacementFor("claude::claude:fresh::claude-opus-5-5", "claude", "claude-opus-5-5", now); p.failover() || p.bindings != nil {
		t.Fatalf("a session without a suspended binding got %+v, want a new placement", p)
	}
}

// failoverTestExecutor serves every credential except the ones it fails with a 429.
func failoverTestExecutor(served *[]string, failing map[string]bool) *mockCustomErrorExecutor {
	return &mockCustomErrorExecutor{
		identifier: "claude",
		executeFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			*served = append(*served, auth.ID)
			if failing[auth.ID] {
				return cliproxyexecutor.Response{}, customStatusError{code: http.StatusTooManyRequests, msg: "rate limited"}
			}
			return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}
}

// In the conductor, a 429 on the bound credential drops the binding before the
// request is retried on another credential. That retry is the session's
// failover: it ranks by stay power, not like a brand-new session.
func TestExpiringFirstFailoverAfterA429Retry(t *testing.T) {
	previousCooldown := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousCooldown) })
	hook := efLogHook(t, log.InfoLevel)

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newHeadroomTestSelector(), TTL: time.Hour})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	var served []string
	failing := map[string]bool{}
	manager.RegisterExecutor(failoverTestExecutor(&served, failing))
	const model = "claude-opus-5-5"
	prefix := "ef-429-retry-"
	urgent, busy, roomy := prefix+"a", prefix+"b", prefix+"c"
	for _, auth := range []*Auth{
		efClaudeAuth(urgent, efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 40, 3 * efHour}), // 13.3%/h
		// B is the next most urgent, so a new session would go there, but only
		// 30% of its 5h window is left; C has 90%.
		efClaudeAuth(busy, efWindow{"5h", 30, 3 * efHour}, efWindow{"7d", 40, 10 * efHour}), // 4%/h
		efClaudeAuth(roomy, efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 90, 6 * efDay}),  // 0.63%/h
	} {
		id := auth.ID
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	execute := func(stage string) {
		t.Helper()
		opts := efSessionOpts("retry-session")
		if _, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, opts); err != nil {
			t.Fatalf("%s: Execute() error = %v", stage, err)
		}
	}

	execute("first request")
	failing[urgent] = true
	hook.Reset()
	execute("request after the 429")
	execute("next request")
	if got := strings.Join(served, ","); got != strings.Join([]string{urgent, urgent, roomy, roomy}, ",") {
		t.Fatalf("served by %s; want the retry and the next request on %s", got, roomy)
	}
	var failover string
	for _, line := range efAffinityLogs(hook) {
		if strings.HasPrefix(line, "session-affinity: bound auth failed, reselected") {
			failover = line
		}
		if strings.HasPrefix(line, "session-affinity: cache miss, new binding") {
			t.Fatalf("the retry was placed as a new session: %q", line)
		}
	}
	if !strings.Contains(failover, " from="+urgent+" auth="+roomy+" ") || !strings.HasSuffix(failover, " reason=failover-stay-power") {
		t.Fatalf("failover line = %q", failover)
	}
}

// A credential failure still releases the binding, as upstream does, but the
// session's next pick is its failover, whichever request makes it, and the
// failover ends the suspended binding.
func TestExpiringFirstFailoverAfterAReleasedBinding(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newHeadroomTestSelector(), TTL: time.Hour})
	defer selector.Stop()
	a := efClaudeAuth("auth-a", efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 40, 3 * efHour})
	// B is far more urgent from now on, but two other sessions use it.
	b := efClaudeAuth("auth-b", efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 90, efHour})
	const key = "claude::claude:freed::claude-opus-5-5"
	opts := efSessionOpts("freed")
	if got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{a}); err != nil || got.ID != "auth-a" {
		t.Fatalf("Pick() = %+v, %v", got, err)
	}
	for _, other := range []string{"other-1", "other-2"} {
		if got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts(other), []*Auth{b}); err != nil || got.ID != "auth-b" {
			t.Fatalf("Pick(%s) = %+v, %v", other, got, err)
		}
	}
	selector.OnResult(Result{AuthID: "auth-a", Provider: "claude", Model: "claude-opus-5-5", Success: false, Options: opts})
	if bound, ok := selector.cache.Get(key); ok {
		t.Fatalf("the failure kept the binding on %s", bound)
	}
	hook.Reset()
	got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts("freed"), []*Auth{a, b})
	if err != nil || got.ID != "auth-a" {
		t.Fatalf("Pick() after the failure = %+v, %v; want auth-a by stay power (90 against 90/3)", got, err)
	}
	if lines := efAffinityLogs(hook); len(lines) != 1 ||
		lines[0] != "session-affinity: bound auth failed, reselected | session=claude:freed from=auth-a auth=auth-a provider=claude model=claude-opus-5-5 reason=failover-stay-power" {
		t.Fatalf("session-affinity lines = %q", lines)
	}
	if _, ok := selector.devyre.suspendedFor(key, time.Now()); ok {
		t.Fatal("the suspended binding outlived the new one")
	}
	// The next pick is an ordinary cache hit.
	hook.Reset()
	if got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts("freed"), []*Auth{a, b}); err != nil || got.ID != "auth-a" {
		t.Fatalf("Pick() = %+v, %v", got, err)
	}
	if lines := efAffinityLogs(hook); len(lines) != 1 || !strings.HasPrefix(lines[0], "session-affinity: cache hit | ") {
		t.Fatalf("session-affinity lines = %q", lines)
	}
}

// Sessions bound to a credential that is removed fail over.
func TestExpiringFirstFailoverForARemovedCredential(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newHeadroomTestSelector(), TTL: time.Hour})
	defer selector.Stop()
	a := efClaudeAuth("auth-a", efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 40, 3 * efHour})
	b := efClaudeAuth("auth-b", efWindow{"5h", 90, 3 * efHour}, efWindow{"7d", 90, efDay})
	if got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts("gone-session"), []*Auth{a, b}); err != nil || got.ID != "auth-a" {
		t.Fatalf("Pick() = %+v, %v", got, err)
	}
	selector.InvalidateAuth("auth-a")
	hook.Reset()
	if got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts("gone-session"), []*Auth{b}); err != nil || got.ID != "auth-b" {
		t.Fatalf("Pick() after removal = %+v, %v", got, err)
	}
	if lines := efAffinityLogs(hook); len(lines) != 1 ||
		lines[0] != "session-affinity: bound auth failed, reselected | session=claude:gone-session from=auth-a auth=auth-b provider=claude model=claude-opus-5-5 reason=failover-stay-power" {
		t.Fatalf("session-affinity lines = %q", lines)
	}
}

func TestExpiringFirstFailoverSuspendedBindings(t *testing.T) {
	t.Parallel()

	now := time.Now()
	var state devyreAffinityState
	state.suspend("k1", "auth-a", now.Add(time.Minute), now)
	state.suspend("k2", "auth-b", now.Add(-time.Second), now)
	if authID, ok := state.suspendedFor("k1", now); !ok || authID != "auth-a" {
		t.Fatalf("suspendedFor(k1) = %q, %v", authID, ok)
	}
	if _, ok := state.suspendedFor("k2", now); ok {
		t.Fatal("an expired suspended binding counted")
	}
	if _, ok := state.suspendedFor("k1", now.Add(time.Minute)); ok {
		t.Fatal("a suspended binding outlived its binding")
	}
	state.suspend("k1", "auth-a", now.Add(time.Minute), now)
	state.forget("k1")
	if _, ok := state.suspendedFor("k1", now); ok {
		t.Fatal("forget kept the suspended binding")
	}

	// The map is bounded: expired entries go first, then everything.
	for i := 0; i < maxSuspendedBindings; i++ {
		state.suspend(fmt.Sprintf("bulk-%d", i), "auth-a", now.Add(time.Minute), now)
	}
	state.suspend("one-more", "auth-a", now.Add(time.Minute), now)
	if len(state.suspended) != 1 {
		t.Fatalf("suspended bindings = %d after overflow, want 1", len(state.suspended))
	}
}

func TestDevyreSessionCacheActiveBindings(t *testing.T) {
	t.Parallel()

	cache := NewSessionCacheWithCapacity(time.Hour, 100)
	defer cache.Stop()
	now := time.Now()
	set := func(authID string, lastUse time.Duration, aliases ...string) {
		cache.mu.Lock()
		cache.replaceAliasGroupsLocked(authID, now.Add(-lastUse).Add(time.Hour), aliases)
		cache.mu.Unlock()
	}
	set("auth-a", time.Minute, "mixed::claude:s1::claude-opus-5-5", "mixed::claude:s1-alias::claude-opus-5-5")
	set("auth-a", 14*time.Minute, "mixed::claude:s2::claude-opus-5-5")
	set("auth-a", 16*time.Minute, "mixed::claude:s3::claude-opus-5-5")        // idle
	set("auth-b", 2*time.Minute, "mixed::claude:s4::claude-opus-5-5")         // another credential
	set("auth-b", 2*time.Minute, "claude::claude:s5::claude-opus-5-5")        // another provider key
	set("auth-b", 2*time.Minute, "mixed::claude:s6::claude-sonnet-5")         // another model
	set("auth-b", 61*time.Minute, "mixed::claude:expired::claude-opus-5-5")   // expired
	set("auth-c", 2*time.Minute, "mixed::claude:s7::claude-opus-5-5-preview") // a model that only starts the same
	got := cache.devyreActiveBindings("mixed::", "::claude-opus-5-5", 15*time.Minute, now)
	if len(got) != 2 || got["auth-a"] != 2 || got["auth-b"] != 1 {
		t.Fatalf("active bindings = %v, want auth-a: 2 (aliases count once), auth-b: 1", got)
	}
	if got := (*SessionCache)(nil).devyreActiveBindings("", "", time.Minute, now); got != nil {
		t.Fatalf("nil cache counts %v", got)
	}
}
