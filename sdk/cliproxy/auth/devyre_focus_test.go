package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// efFocusStore returns an in-memory focus store with each of auths focused for
// its provider, without an end.
func efFocusStore(t *testing.T, auths ...*Auth) *routingfocus.Store {
	t.Helper()
	store := routingfocus.NewStore()
	for _, auth := range auths {
		efSetFocus(t, store, auth)
	}
	return store
}

func efSetFocus(t *testing.T, store *routingfocus.Store, auth *Auth) {
	t.Helper()
	if err := store.Set(routingfocus.Focus{AuthID: auth.ID, Provider: auth.Provider}); err != nil {
		t.Fatalf("focus %s: %v", auth.ID, err)
	}
}

func newTestFocusSelector(focus *routingfocus.Store) *ExpiringFirstSelector {
	selector := newTestExpiringFirstSelector()
	selector.Focus = focus
	return selector
}

func TestExpiringFirstSelectorFocus(t *testing.T) {
	t.Parallel()

	urgent := func() *Auth { return efClaudeAuth("a-urgent", efWindow{"7d", 40, 3 * efHour}) } // 13.3%/h
	focused := func(windows ...efWindow) *Auth {
		if len(windows) == 0 {
			windows = []efWindow{{"7d", 90, 6 * efDay}} // 0.6%/h
		}
		return efClaudeAuth("f-focused", windows...)
	}
	tests := []struct {
		name       string
		provider   string
		model      string
		auths      []*Auth
		focus      []int // indexes into auths
		otherFocus []*Auth
		wantID     string
		wantReason string
	}{
		{
			name:  "usable focus wins over a higher urgency",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), focused()},
			focus: []int{1}, wantID: "f-focused", wantReason: expiringFirstReasonFocus,
		},
		{
			name:  "focus without readings wins",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), efClaudeAuth("f-focused")},
			focus: []int{1}, wantID: "f-focused", wantReason: expiringFirstReasonFocus,
		},
		{
			name:  "focus gated by its 5h window is ignored",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), focused(efWindow{"5h", 1, efHour}, efWindow{"7d", 90, 6 * efDay})},
			focus: []int{1}, wantID: "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
		{
			name:  "focus gated by its 7d window is ignored",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), focused(efWindow{"7d", 0, efDay})},
			focus: []int{1}, wantID: "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
		{
			name:  "focus gated for the requested model family is ignored",
			model: "claude-fable-5",
			auths: []*Auth{urgent(), focused(efWindow{"7d", 90, 6 * efDay}, efWindow{"7d_oi", 0, 2 * efDay})},
			focus: []int{1}, wantID: "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
		{
			name:  "a model-scoped gate leaves other models on the focus",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), focused(efWindow{"7d", 90, 6 * efDay}, efWindow{"7d_oi", 0, 2 * efDay})},
			focus: []int{1}, wantID: "f-focused", wantReason: expiringFirstReasonFocus,
		},
		{
			name:  "focus below the top priority tier is ignored",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), efWithPriority(focused(), -1)},
			focus: []int{1}, wantID: "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
		{
			name:  "focus in the top priority tier wins over a lower tier",
			model: "claude-opus-5-5",
			auths: []*Auth{efWithPriority(urgent(), -1), focused()},
			focus: []int{1}, wantID: "f-focused", wantReason: expiringFirstReasonFocus,
		},
		{
			name:  "unavailable focus is ignored",
			model: "claude-opus-5-5",
			auths: []*Auth{urgent(), func() *Auth {
				auth := focused()
				auth.Unavailable = true
				return auth
			}()},
			focus: []int{1}, wantID: "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
		{
			name:       "another provider's focus is ignored",
			model:      "claude-opus-5-5",
			auths:      []*Auth{urgent(), focused()},
			otherFocus: []*Auth{efCodexAuth("codex-focused", 90, 6*efDay, false)},
			wantID:     "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
		{
			name:       "a focus outside the candidates is ignored",
			model:      "claude-opus-5-5",
			auths:      []*Auth{urgent(), focused()},
			otherFocus: []*Auth{efClaudeAuth("removed-claude")},
			wantID:     "a-urgent", wantReason: expiringFirstReasonMostUrgent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := efFocusStore(t, tc.otherFocus...)
			for _, index := range tc.focus {
				efSetFocus(t, store, tc.auths[index])
			}
			provider := tc.provider
			if provider == "" {
				provider = "claude"
			}
			selector := newTestFocusSelector(store)
			for round := 0; round < 3; round++ {
				picked, reason, err := selector.pick(context.Background(), provider, tc.model, tc.auths)
				if err != nil {
					t.Fatalf("pick() error = %v", err)
				}
				if picked.auth.ID != tc.wantID || reason != tc.wantReason {
					t.Fatalf("pick() #%d = %s reason %q, want %s reason %q", round, picked.auth.ID, reason, tc.wantID, tc.wantReason)
				}
			}
		})
	}
}

func TestExpiringFirstSelectorFocusRotatesAcrossProviders(t *testing.T) {
	t.Parallel()

	claude := efClaudeAuth("claude-focused", efWindow{"7d", 90, 6 * efDay})
	codex := efCodexAuth("codex-focused", 90, 6*efDay, false)
	urgent := efClaudeAuth("claude-urgent", efWindow{"7d", 40, 3 * efHour})
	selector := newTestFocusSelector(efFocusStore(t, claude, codex))
	var picks []string
	for round := 0; round < 4; round++ {
		picked, reason, err := selector.pick(context.Background(), "mixed", "gpt-5.5", []*Auth{urgent, codex, claude})
		if err != nil || reason != expiringFirstReasonFocus {
			t.Fatalf("pick() = %+v %q %v", picked.auth, reason, err)
		}
		picks = append(picks, picked.auth.ID)
	}
	if got := strings.Join(picks, ","); got != "claude-focused,codex-focused,claude-focused,codex-focused" {
		t.Fatalf("picks = %s, want the two focuses in turn", got)
	}
}

func TestExpiringFirstSelectorFocusEnds(t *testing.T) {
	t.Parallel()

	// The selector's clock decides; readings are left out so that moving it
	// changes nothing else: both credentials are unknowns.
	until := time.Now().Add(time.Hour).UTC()
	store := routingfocus.NewStore()
	focused := &Auth{ID: "z-focused", Provider: "claude"}
	if err := store.Set(routingfocus.Focus{AuthID: focused.ID, Provider: "claude", Until: until}); err != nil {
		t.Fatal(err)
	}
	auths := []*Auth{{ID: "a-plain", Provider: "claude"}, focused}
	for _, tc := range []struct {
		now        time.Time
		wantID     string
		wantReason string
	}{
		{until.Add(-time.Second), "z-focused", expiringFirstReasonFocus},
		{until, "a-plain", expiringFirstReasonNoData},
		{until.Add(time.Hour), "a-plain", expiringFirstReasonNoData},
	} {
		selector := &ExpiringFirstSelector{GateRemainingPercent: 2, Now: func() time.Time { return tc.now }, Focus: store}
		picked, reason, err := selector.pick(context.Background(), "claude", "claude-opus-5-5", auths)
		if err != nil || picked.auth.ID != tc.wantID || reason != tc.wantReason {
			t.Fatalf("pick() at %s = %+v %q %v; want %s %q", tc.now, picked.auth, reason, err, tc.wantID, tc.wantReason)
		}
	}
}

func TestExpiringFirstSelectorLogsFocusPicks(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	focused := efClaudeAuth("auth-focused", efWindow{"7d", 90, 6 * efDay})
	selector := newTestFocusSelector(efFocusStore(t, focused))
	selector.LogPicks = true
	auths := []*Auth{efClaudeAuth("auth-urgent", efWindow{"7d", 40, 3 * efHour}), focused}
	if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, auths); err != nil {
		t.Fatal(err)
	}
	entries := efPickEntries(hook)
	if len(entries) != 1 || entries[0].Data["reason"] != "focus" || entries[0].Data["auth"] != "auth-focused" ||
		entries[0].Message != "expiring-first: pick | auth=auth-focused urgency=0.625 window=7d" {
		t.Fatalf("pick log entries = %+v", entries)
	}
}

// efSessionOptions carries an explicit Claude Code session.
func efSessionOptions(session string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": {session}},
		Metadata: make(map[string]any),
	}
}

func TestSessionAffinityFocusTakesOverBoundSessions(t *testing.T) {
	t.Parallel()

	const model = "claude-opus-5-5"
	tests := []struct {
		name string
		// focus changes the pool once the session is bound to b-urgent.
		focus  func(t *testing.T, store *routingfocus.Store, a, b *Auth)
		wantID string
	}{
		{
			name:   "a usable focus takes the session over",
			focus:  func(t *testing.T, store *routingfocus.Store, a, _ *Auth) { efSetFocus(t, store, a) },
			wantID: "a-relaxed",
		},
		{
			name:   "nothing focused keeps the binding",
			focus:  func(*testing.T, *routingfocus.Store, *Auth, *Auth) {},
			wantID: "b-urgent",
		},
		{
			name:   "a focus on the bound credential keeps the binding",
			focus:  func(t *testing.T, store *routingfocus.Store, _, b *Auth) { efSetFocus(t, store, b) },
			wantID: "b-urgent",
		},
		{
			name: "a gated focus keeps the binding",
			focus: func(t *testing.T, store *routingfocus.Store, a, _ *Auth) {
				a.Quota.Signals = efClaudeSignals(efWindow{"5h", 0, efHour}, efWindow{"7d", 90, 6 * efDay})
				efSetFocus(t, store, a)
			},
			wantID: "b-urgent",
		},
		{
			name: "a focus below the top tier keeps the binding",
			focus: func(t *testing.T, store *routingfocus.Store, a, _ *Auth) {
				efWithPriority(a, -1)
				efSetFocus(t, store, a)
			},
			wantID: "b-urgent",
		},
		{
			name: "another provider's focus keeps the binding",
			focus: func(t *testing.T, store *routingfocus.Store, _, _ *Auth) {
				efSetFocus(t, store, efCodexAuth("codex-focused", 90, 6*efDay, false))
			},
			wantID: "b-urgent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := routingfocus.NewStore()
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newTestFocusSelector(store), TTL: time.Hour})
			defer selector.Stop()
			a := efClaudeAuth("a-relaxed", efWindow{"7d", 90, 6 * efDay}) // 0.6%/h
			b := efClaudeAuth("b-urgent", efWindow{"7d", 40, 3 * efHour}) // 13.3%/h
			auths := []*Auth{a, b}
			pick := func(stage, session string) string {
				t.Helper()
				got, err := selector.Pick(context.Background(), "claude", model, efSessionOptions(session), auths)
				if err != nil || got == nil {
					t.Fatalf("%s: Pick() = %+v, %v", stage, got, err)
				}
				return got.ID
			}

			if got := pick("new session", "sess-focus"); got != "b-urgent" {
				t.Fatalf("new session bound to %s, want b-urgent", got)
			}
			tc.focus(t, store, a, b)
			if got := pick("bound session", "sess-focus"); got != tc.wantID {
				t.Fatalf("bound session picked %s, want %s", got, tc.wantID)
			}
			if got, status := selector.LookupAffinity("claude", model, "sess-focus"); got != tc.wantID || status != "bound" {
				t.Fatalf("LookupAffinity() = %s %s, want %s bound", got, status, tc.wantID)
			}
			// Without the focus the session stays where it is now bound.
			if err := store.ClearAuth(a.ID); err != nil {
				t.Fatal(err)
			}
			if got := pick("after the focus", "sess-focus"); got != tc.wantID {
				t.Fatalf("session moved to %s after the focus was cleared, want %s", got, tc.wantID)
			}
		})
	}
}

func TestSessionAffinityFocusTakesOverFallbackKey(t *testing.T) {
	t.Parallel()

	const model = "claude-opus-5-5"
	// The conversation ID is the fallback session of a request that also
	// carries a prompt cache key, so a new cache key reaches the binding
	// through the fallback key.
	request := func(cacheKey string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":{"id":"conv-focus"},"prompt_cache_key":"` + cacheKey + `"}`)}
	}
	for _, tc := range []struct {
		name   string
		focus  bool
		wantID string
	}{
		{name: "a usable focus takes the fallback-key session over", focus: true, wantID: "a-relaxed"},
		{name: "nothing focused keeps the fallback-key binding", wantID: "b-urgent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := routingfocus.NewStore()
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newTestFocusSelector(store), TTL: time.Hour})
			defer selector.Stop()
			a := efClaudeAuth("a-relaxed", efWindow{"7d", 90, 6 * efDay})
			b := efClaudeAuth("b-urgent", efWindow{"7d", 40, 3 * efHour})
			auths := []*Auth{a, b}
			pick := func(stage string, opts cliproxyexecutor.Options) string {
				t.Helper()
				got, err := selector.Pick(context.Background(), "claude", model, opts, auths)
				if err != nil || got == nil {
					t.Fatalf("%s: Pick() = %+v, %v", stage, got, err)
				}
				return got.ID
			}

			first := request("bucket-one")
			if primary, fallback := extractSessionIDs(first.Headers, first.OriginalRequest, first.Metadata); fallback == "" || fallback == primary {
				t.Fatalf("request has no separate fallback session (primary %q, fallback %q)", primary, fallback)
			}
			if got := pick("new session", first); got != "b-urgent" {
				t.Fatalf("new session bound to %s, want b-urgent", got)
			}
			if tc.focus {
				efSetFocus(t, store, a)
			}
			if got := pick("new cache key", request("bucket-two")); got != tc.wantID {
				t.Fatalf("fallback-key session picked %s, want %s", got, tc.wantID)
			}
			// The move rebinds the conversation: it stays without the focus.
			if err := store.ClearAuth(a.ID); err != nil {
				t.Fatal(err)
			}
			if got := pick("after the focus", request("bucket-three")); got != tc.wantID {
				t.Fatalf("after the focus the session moved to %s, want %s", got, tc.wantID)
			}
		})
	}
}

func TestSessionAffinityFocusTakesOverLCPSessions(t *testing.T) {
	t.Parallel()

	const model = "claude-opus-5-5"
	turns := []string{
		`{"role":"system","content":"stable"},{"role":"user","content":"first"}`,
		`{"role":"system","content":"stable"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}`,
		`{"role":"system","content":"stable"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"},{"role":"assistant","content":"more"},{"role":"user","content":"again"}`,
	}
	lcpOptions := func(turn int) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			OriginalRequest: []byte(`{"messages":[` + turns[turn] + `]}`),
			Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller-focus"},
		}
	}
	for _, tc := range []struct {
		name     string
		focus    bool
		gated    bool
		wantMove string
	}{
		{name: "a usable focus takes the LCP session over", focus: true, wantMove: "a-relaxed"},
		{name: "nothing focused keeps the LCP binding", wantMove: "b-urgent"},
		{name: "a gated focus keeps the LCP binding", focus: true, gated: true, wantMove: "b-urgent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := routingfocus.NewStore()
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newTestFocusSelector(store), TTL: time.Hour})
			defer selector.Stop()
			a := efClaudeAuth("a-relaxed", efWindow{"7d", 90, 6 * efDay})
			b := efClaudeAuth("b-urgent", efWindow{"7d", 40, 3 * efHour})
			auths := []*Auth{a, b}
			pick := func(stage string, turn int) (string, any) {
				t.Helper()
				opts := lcpOptions(turn)
				got, err := selector.Pick(context.Background(), "claude", model, opts, auths)
				if err != nil || got == nil {
					t.Fatalf("%s: Pick() = %+v, %v", stage, got, err)
				}
				return got.ID, opts.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey]
			}

			got, session := pick("first turn", 0)
			if got != "b-urgent" || session == nil || session == "" {
				t.Fatalf("first turn = %s (session %v), want b-urgent with an LCP session", got, session)
			}
			if tc.gated {
				a.Quota.Signals = efClaudeSignals(efWindow{"7d", 1, 2 * efDay})
			}
			if tc.focus {
				efSetFocus(t, store, a)
			}
			got, grownSession := pick("grown turn", 1)
			if got != tc.wantMove || grownSession != session {
				t.Fatalf("grown turn = %s (session %v), want %s in session %v", got, grownSession, tc.wantMove, session)
			}
			if err := store.ClearAuth(a.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ = pick("after the focus", 2); got != tc.wantMove {
				t.Fatalf("after the focus the session moved to %s, want %s", got, tc.wantMove)
			}
		})
	}
}

func TestSessionAffinityFocusTakeoverIsLogged(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	store := routingfocus.NewStore()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newTestFocusSelector(store), TTL: time.Hour})
	defer selector.Stop()
	const secret = "sk-ant-oat01-focus-log-secret"
	a := efClaudeAuth("a-relaxed", efWindow{"7d", 90, 6 * efDay})
	a.Metadata = map[string]any{"access_token": secret}
	b := efClaudeAuth("b-urgent", efWindow{"7d", 40, 3 * efHour})
	auths := []*Auth{a, b}
	if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOptions("sess-log"), auths); err != nil {
		t.Fatal(err)
	}
	efSetFocus(t, store, a)
	if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOptions("sess-log"), auths); err != nil {
		t.Fatal(err)
	}
	var moves []*log.Entry
	for _, entry := range hook.AllEntries() {
		if strings.HasPrefix(entry.Message, "session-affinity: focus takes over") {
			moves = append(moves, entry)
		}
		if strings.Contains(entry.Message, secret) {
			t.Fatal("session-affinity log leaked a token")
		}
	}
	const want = "session-affinity: focus takes over the binding | session=claude:sess-log from=b-urgent focus=a-relaxed provider=claude model=claude-opus-5-5"
	if len(moves) != 1 || moves[0].Level != log.InfoLevel || moves[0].Message != want {
		t.Fatalf("focus log entries = %+v, want one info entry %q", moves, want)
	}
}

func TestSessionAffinityFocusThroughManager(t *testing.T) {
	t.Parallel()

	store := routingfocus.NewStore()
	selector := NewSessionAffinitySelector(newTestFocusSelector(store))
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	relaxed := efClaudeAuth("ef-focus-manager-relaxed", efWindow{"7d", 90, 6 * efDay})
	urgent := efClaudeAuth("ef-focus-manager-urgent", efWindow{"7d", 40, 3 * efHour})
	for _, auth := range []*Auth{relaxed, urgent} {
		id := auth.ID
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_5c1e7a3d-9b2f-4d6e-8a0c-3f7b1e5d9a42"}}`)}
	selectID := func(stage string) string {
		t.Helper()
		got, err := manager.SelectAuth(context.Background(), "claude", "claude-opus-5-5", opts)
		if err != nil || got == nil {
			t.Fatalf("%s: SelectAuth() = %+v, %v", stage, got, err)
		}
		return got.ID
	}
	if got := selectID("new session"); got != urgent.ID {
		t.Fatalf("new session bound to %s, want %s", got, urgent.ID)
	}
	efSetFocus(t, store, relaxed)
	for round := 0; round < 2; round++ {
		if got := selectID("focused"); got != relaxed.ID {
			t.Fatalf("focused SelectAuth() #%d = %s, want the focus %s", round, got, relaxed.ID)
		}
	}
}
