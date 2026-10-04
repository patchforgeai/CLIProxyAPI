package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// efTestNow is the fixed clock of the expiring-first selector tests.
var efTestNow = time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)

const (
	efHour = time.Hour
	efDay  = 24 * time.Hour
)

// efWindow is one Claude unified rate-limit window relative to efTestNow.
type efWindow struct {
	name      string        // header window name: "5h", "7d" or "7d_oi" (Fable)
	remaining float64       // percent left
	resetsIn  time.Duration // until the reset; negative when the reset already passed
}

// efClaudeSignals renders windows the way collectQuotaSignals stores the
// anthropic-ratelimit-unified-* response headers: canonical names, the
// utilization as a fraction and the reset as unix seconds.
func efClaudeSignals(windows ...efWindow) map[string]string {
	if len(windows) == 0 {
		return nil
	}
	signals := make(map[string]string, 3*len(windows))
	for _, window := range windows {
		prefix := "Anthropic-Ratelimit-Unified-" + window.name + "-"
		signals[prefix+"Utilization"] = strconv.FormatFloat((100-window.remaining)/100, 'f', -1, 64)
		signals[prefix+"Reset"] = strconv.FormatInt(efTestNow.Add(window.resetsIn).Unix(), 10)
		signals[prefix+"Status"] = "allowed"
	}
	return signals
}

// efClaudeAuth builds a Claude credential whose quota readings come only from
// its header snapshot; no windows means no readings at all.
func efClaudeAuth(id string, windows ...efWindow) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota:    QuotaState{ObservedAt: efTestNow, Signals: efClaudeSignals(windows...)},
	}
}

func efWithPriority(auth *Auth, priority int) *Auth {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["priority"] = strconv.Itoa(priority)
	return auth
}

// efCodexAuth builds a Codex credential with a 5-hour primary window that has
// plenty left and a weekly secondary window that ranks it.
func efCodexAuth(id string, secondaryRemaining float64, secondaryResetsIn time.Duration, websockets bool) *Auth {
	auth := &Auth{
		ID:       id,
		Provider: "codex",
		Quota: QuotaState{ObservedAt: efTestNow, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":     "10",
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         strconv.FormatInt(efTestNow.Add(2*efHour).Unix(), 10),
			"X-Codex-Secondary-Used-Percent":   strconv.FormatFloat(100-secondaryRemaining, 'f', -1, 64),
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       strconv.FormatInt(efTestNow.Add(secondaryResetsIn).Unix(), 10),
		}},
	}
	if websockets {
		auth.Attributes = map[string]string{"websockets": "true"}
	}
	return auth
}

func newTestExpiringFirstSelector() *ExpiringFirstSelector {
	return &ExpiringFirstSelector{GateRemainingPercent: 2, Now: func() time.Time { return efTestNow }}
}

func efApprox(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

func TestExpiringFirstSelectorPick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		model       string
		auths       []*Auth
		wantID      string
		wantReason  string
		wantUrgency float64 // 0 means the pick has no known urgency
	}{
		{
			// A: 40% left resetting in 3h is lost sooner than B: 90% left for 4 days.
			name:  "A beats B",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("b", efWindow{"7d", 90, 4 * efDay}),
				efClaudeAuth("a", efWindow{"7d", 40, 3 * efHour}),
			},
			wantID: "a", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 40.0 / 3,
		},
		{
			// D: 80% left in 2h outranks C: 1% left in 10 minutes, floored to 15 minutes.
			name:  "D beats C",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("c", efWindow{"7d", 1, 10 * time.Minute}),
				efClaudeAuth("d", efWindow{"7d", 80, 2 * efHour}),
			},
			wantID: "d", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 40,
		},
		{
			name:  "exhausted 5h window gates",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 0, efHour}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("b", efWindow{"7d", 20, 6 * efDay}),
			},
			wantID: "b", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 20.0 / 144,
		},
		{
			name:  "5h window at the gate threshold gates",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 2, efHour}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("b", efWindow{"7d", 20, 6 * efDay}),
			},
			wantID: "b", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 20.0 / 144,
		},
		{
			name:  "5h window above the gate threshold does not gate",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 3, efHour}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("b", efWindow{"7d", 20, 6 * efDay}),
			},
			wantID: "a", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 50.0 / 24,
		},
		{
			name:  "exhausted Fable window gates Fable models",
			model: "claude-fable-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"7d", 50, efDay}, efWindow{"7d_oi", 0, 2 * efDay}),
				efClaudeAuth("b", efWindow{"7d", 20, 6 * efDay}),
			},
			wantID: "b", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 20.0 / 144,
		},
		{
			name:  "exhausted Fable window does not gate other models",
			model: "claude-opus-5-5(high)",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"7d", 50, efDay}, efWindow{"7d_oi", 0, 2 * efDay}),
				efClaudeAuth("b", efWindow{"7d", 20, 6 * efDay}),
			},
			wantID: "a", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 50.0 / 24,
		},
		{
			// A's weekly window reset an hour ago: it is a full window that next
			// resets in 167h, which still outranks B's half window over 100h.
			name:  "passed reset counts as a full window",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"7d", 0, -efHour}),
				efClaudeAuth("b", efWindow{"7d", 50, 100 * efHour}),
			},
			wantID: "a", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 100.0 / 167,
		},
		{
			name:  "passed reset has low urgency",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"7d", 0, -efHour}),
				efClaudeAuth("c", efWindow{"7d", 40, 3 * efHour}),
			},
			wantID: "c", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 40.0 / 3,
		},
		{
			name:  "passed 5h reset no longer gates",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 0, -time.Minute}, efWindow{"7d", 50, efDay}),
				efClaudeAuth("b", efWindow{"7d", 20, 6 * efDay}),
			},
			wantID: "a", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 50.0 / 24,
		},
		{
			name:  "known urgency ranks before missing data",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a-unknown"),
				efClaudeAuth("k", efWindow{"7d", 90, 6 * efDay}),
			},
			wantID: "k", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 90.0 / 144,
		},
		{
			// Without a long window the longest known window ranks the credential.
			name:  "short window ranks when no long window is known",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 80, efHour}),
				efClaudeAuth("b"),
			},
			wantID: "a", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 80,
		},
		{
			name:  "missing data is picked when every known candidate is gated",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("k", efWindow{"5h", 0, efHour}, efWindow{"7d", 90, 6 * efDay}),
				efClaudeAuth("u"),
			},
			wantID: "u", wantReason: expiringFirstReasonNoData,
		},
		{
			name:  "highest priority tier is respected",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efWithPriority(efClaudeAuth("high", efWindow{"7d", 90, 6 * efDay}), 10),
				efWithPriority(efClaudeAuth("low", efWindow{"7d", 40, 3 * efHour}), 0),
			},
			wantID: "high", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 90.0 / 144,
		},
		{
			// A gated top tier is not abandoned for a lower tier: upstream 429s
			// and cooldowns move traffic once the credential is really unavailable.
			name:  "gated top tier stays ahead of lower tiers",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efWithPriority(efClaudeAuth("high", efWindow{"5h", 0, efHour}, efWindow{"7d", 90, 6 * efDay}), 10),
				efWithPriority(efClaudeAuth("low", efWindow{"7d", 40, 3 * efHour}), 0),
			},
			wantID: "high", wantReason: expiringFirstReasonAllGated, wantUrgency: 90.0 / 144,
		},
		{
			name:  "all gated falls back to the earliest gate reset",
			model: "claude-opus-5-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 0, 2 * efHour}, efWindow{"7d", 90, 6 * efDay}),
				efClaudeAuth("b", efWindow{"5h", 1, 30 * time.Minute}, efWindow{"7d", 10, 6 * efDay}),
				efClaudeAuth("c", efWindow{"5h", 0, efHour}, efWindow{"7d", 50, efDay}),
			},
			wantID: "b", wantReason: expiringFirstReasonAllGated, wantUrgency: 10.0 / 144,
		},
		{
			// A frees its 5h window first, but its Fable window keeps gating Fable
			// requests for days, so B, which frees in 2h, is the earliest usable.
			name:  "all gated waits for the latest gate of each credential",
			model: "claude-fable-5",
			auths: []*Auth{
				efClaudeAuth("a", efWindow{"5h", 0, 30 * time.Minute}, efWindow{"7d", 90, 6 * efDay}, efWindow{"7d_oi", 0, 3 * efDay}),
				efClaudeAuth("b", efWindow{"5h", 0, 2 * efHour}, efWindow{"7d", 90, 6 * efDay}),
			},
			wantID: "b", wantReason: expiringFirstReasonAllGated, wantUrgency: 90.0 / 144,
		},
		{
			name:  "unavailable credential is skipped",
			model: "claude-opus-5-5",
			auths: []*Auth{
				func() *Auth {
					auth := efClaudeAuth("a", efWindow{"7d", 40, 3 * efHour})
					auth.Unavailable = true
					return auth
				}(),
				efClaudeAuth("b", efWindow{"7d", 90, 4 * efDay}),
			},
			wantID: "b", wantReason: expiringFirstReasonMostUrgent, wantUrgency: 90.0 / 96,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			selector := newTestExpiringFirstSelector()
			picked, reason, err := selector.pick(context.Background(), "claude", tc.model, tc.auths)
			if err != nil {
				t.Fatalf("pick() error = %v", err)
			}
			if picked.auth == nil || picked.auth.ID != tc.wantID || reason != tc.wantReason {
				t.Fatalf("pick() = %+v reason %q, want %s reason %q", picked.auth, reason, tc.wantID, tc.wantReason)
			}
			if picked.eval.UrgencyKnown != (tc.wantUrgency != 0) || !efApprox(picked.eval.Urgency, tc.wantUrgency) {
				t.Fatalf("urgency = %v (known %t), want %v", picked.eval.Urgency, picked.eval.UrgencyKnown, tc.wantUrgency)
			}
			got, errPick := newTestExpiringFirstSelector().Pick(context.Background(), "claude", tc.model, cliproxyexecutor.Options{}, tc.auths)
			if errPick != nil || got == nil || got.ID != tc.wantID {
				t.Fatalf("Pick() = %+v, %v; want %s", got, errPick, tc.wantID)
			}
		})
	}
}

func TestExpiringFirstSelectorRotatesTiesAndUnknowns(t *testing.T) {
	t.Parallel()

	pickIDs := func(t *testing.T, selector *ExpiringFirstSelector, model string, auths []*Auth, count int) []string {
		t.Helper()
		ids := make([]string, 0, count)
		for index := 0; index < count; index++ {
			got, err := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
			if err != nil {
				t.Fatalf("Pick() #%d error = %v", index, err)
			}
			ids = append(ids, got.ID)
		}
		return ids
	}

	t.Run("equal urgencies rotate by ID", func(t *testing.T) {
		t.Parallel()
		selector := newTestExpiringFirstSelector()
		auths := []*Auth{
			efClaudeAuth("t2", efWindow{"7d", 50, efDay}),
			efClaudeAuth("t1", efWindow{"7d", 50, efDay}),
			efClaudeAuth("slow", efWindow{"7d", 50, 2 * efDay}),
		}
		if got := strings.Join(pickIDs(t, selector, "claude-opus-5-5", auths, 4), ","); got != "t1,t2,t1,t2" {
			t.Fatalf("picks = %s, want t1,t2,t1,t2", got)
		}
	})

	t.Run("unknowns rotate after the gated knowns", func(t *testing.T) {
		t.Parallel()
		selector := newTestExpiringFirstSelector()
		auths := []*Auth{
			efClaudeAuth("u3"),
			efClaudeAuth("gated", efWindow{"5h", 0, efHour}, efWindow{"7d", 90, 6 * efDay}),
			efClaudeAuth("u1"),
			efClaudeAuth("u2"),
		}
		if got := strings.Join(pickIDs(t, selector, "claude-opus-5-5", auths, 4), ","); got != "u1,u2,u3,u1" {
			t.Fatalf("picks = %s, want u1,u2,u3,u1", got)
		}
		// The rotation resumes after the previous pick when candidates drop out.
		if got := strings.Join(pickIDs(t, selector, "claude-opus-5-5", []*Auth{auths[0], auths[2]}, 2), ","); got != "u3,u1" {
			t.Fatalf("picks after u2 dropped = %s, want u3,u1", got)
		}
	})

	t.Run("rotation is kept per provider and model", func(t *testing.T) {
		t.Parallel()
		selector := newTestExpiringFirstSelector()
		auths := []*Auth{efClaudeAuth("u1"), efClaudeAuth("u2")}
		if got := strings.Join(pickIDs(t, selector, "claude-opus-5-5", auths, 1), ","); got != "u1" {
			t.Fatalf("opus picks = %s, want u1", got)
		}
		if got := strings.Join(pickIDs(t, selector, "claude-sonnet-5", auths, 1), ","); got != "u1" {
			t.Fatalf("sonnet picks = %s, want its own rotation starting at u1", got)
		}
		// A thinking suffix shares the base model's rotation.
		if got := strings.Join(pickIDs(t, selector, "claude-opus-5-5(high)", auths, 1), ","); got != "u2" {
			t.Fatalf("opus(high) picks = %s, want u2", got)
		}
	})
}

func TestExpiringFirstSelectorReadsDefaultStore(t *testing.T) {
	t.Parallel()

	usage := func(id string, remaining float64, resetsIn time.Duration, observedAt time.Time) {
		quotareading.Default().Put(id, "claude", []quotareading.Window{{
			ID:          "7d",
			Kind:        quotareading.KindLong,
			UsedPercent: 100 - remaining,
			ResetsAt:    efTestNow.Add(resetsIn),
			Length:      7 * efDay,
			ObservedAt:  observedAt,
			Source:      quotareading.SourceUsage,
		}})
	}
	const storeOnly, headerNewer, headerOlder = "ef-store-test-store-only", "ef-store-test-header-newer", "ef-store-test-header-older"
	t.Cleanup(func() {
		for _, id := range []string{storeOnly, headerNewer, headerOlder} {
			quotareading.Default().Forget(id)
		}
	})

	// A usage poll is the only reading of an idle credential and ranks it.
	usage(storeOnly, 60, 2*efDay, efTestNow.Add(-10*time.Minute))
	selector := newTestExpiringFirstSelector()
	auths := []*Auth{efClaudeAuth(storeOnly), efClaudeAuth("ef-store-test-plain", efWindow{"7d", 90, 6 * efDay})}
	picked, reason, err := selector.pick(context.Background(), "claude", "claude-opus-5-5", auths)
	if err != nil || picked.auth.ID != storeOnly || reason != expiringFirstReasonMostUrgent || !efApprox(picked.eval.Urgency, 60.0/48) {
		t.Fatalf("pick() = %+v %q %v, want the store-ranked credential", picked, reason, err)
	}

	// A newer header snapshot overrides an older usage reading of the same window.
	usage(headerNewer, 90, 6*efDay, efTestNow.Add(-time.Hour))
	newer := efClaudeAuth(headerNewer, efWindow{"7d", 10, 6 * efDay})
	picked, _, err = selector.pick(context.Background(), "claude", "claude-opus-5-5", []*Auth{newer})
	if err != nil || !efApprox(picked.eval.Urgency, 10.0/144) {
		t.Fatalf("pick() = %+v %v, want the newer header reading (10%% left)", picked, err)
	}
	// An older header snapshot loses to a newer usage reading.
	usage(headerOlder, 50, efDay, efTestNow.Add(-time.Minute))
	older := efClaudeAuth(headerOlder, efWindow{"7d", 99, 6 * efDay})
	older.Quota.ObservedAt = efTestNow.Add(-time.Hour)
	picked, _, err = selector.pick(context.Background(), "claude", "claude-opus-5-5", []*Auth{older})
	if err != nil || !efApprox(picked.eval.Urgency, 50.0/24) {
		t.Fatalf("pick() = %+v %v, want the newer usage reading (50%% left)", picked, err)
	}
}

func TestExpiringFirstSelectorPreservesCodexWebsocketPreference(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		efCodexAuth("plain-urgent", 40, 3*efHour, false), // 13.3%/h
		efCodexAuth("ws-relaxed", 90, 6*efDay, true),     // 0.6%/h
		efCodexAuth("ws-mid", 50, 2*efDay, true),         // 1.0%/h
	}
	wsCtx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, err := newTestExpiringFirstSelector().Pick(wsCtx, "codex", "gpt-5.5", cliproxyexecutor.Options{}, auths)
	if err != nil || got == nil || got.ID != "ws-mid" {
		t.Fatalf("websocket Pick() = %+v, %v; want the most urgent websocket credential ws-mid", got, err)
	}
	got, err = newTestExpiringFirstSelector().Pick(context.Background(), "codex", "gpt-5.5", cliproxyexecutor.Options{}, auths)
	if err != nil || got == nil || got.ID != "plain-urgent" {
		t.Fatalf("HTTP Pick() = %+v, %v; want plain-urgent", got, err)
	}
}

func TestExpiringFirstSelectorReturnsCooldownError(t *testing.T) {
	t.Parallel()

	model := "claude-opus-5-5"
	cooling := func(id string) *Auth {
		auth := efClaudeAuth(id, efWindow{"7d", 40, 3 * efHour})
		auth.ModelStates = map[string]*ModelState{model: {
			Status:         StatusActive,
			Unavailable:    true,
			NextRetryAfter: efTestNow.Add(time.Minute),
			Quota:          QuotaState{Exceeded: true, NextRecoverAt: efTestNow.Add(time.Minute)},
		}}
		return auth
	}
	got, err := newTestExpiringFirstSelector().Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, []*Auth{cooling("a"), cooling("b")})
	var cooldownErr *modelCooldownError
	if got != nil || !errors.As(err, &cooldownErr) {
		t.Fatalf("Pick() = %+v, %v; want a model cooldown error", got, err)
	}
}

func TestExpiringFirstSelectorUnderSessionAffinity(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newTestExpiringFirstSelector(), TTL: time.Hour})
	defer selector.Stop()
	a := efClaudeAuth("auth-a", efWindow{"7d", 40, 3 * efHour}) // 13.3%/h
	b := efClaudeAuth("auth-b", efWindow{"7d", 60, efDay})      // 2.5%/h
	c := efClaudeAuth("auth-c", efWindow{"7d", 90, 6 * efDay})  // 0.6%/h
	auths := []*Auth{a, b, c}
	session := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_7b0c6a52-5d0e-4a8e-9a43-1f4d2c9e8b10"}}`)}
	pick := func(stage string, opts cliproxyexecutor.Options) string {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
		if err != nil || got == nil {
			t.Fatalf("%s: Pick() = %+v, %v", stage, got, err)
		}
		return got.ID
	}

	if got := pick("new session", session); got != "auth-a" {
		t.Fatalf("new session bound to %s, want the most urgent auth-a", got)
	}
	// B becomes far more urgent; the bound session must not migrate.
	b.Quota.Signals = efClaudeSignals(efWindow{"7d", 90, efHour})
	if got := pick("bound session", session); got != "auth-a" {
		t.Fatalf("bound session moved to %s, want auth-a", got)
	}
	// A session without a binding goes to the new most urgent credential.
	other := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_2f6e9c1a-8b3d-4c7e-a5f0-6d1b9e4c3a27"}}`)}
	if got := pick("second session", other); got != "auth-b" {
		t.Fatalf("second session bound to %s, want auth-b", got)
	}
	// When the bound credential becomes unavailable, failover picks the most
	// urgent of the rest (B over C) and the session stays there afterwards.
	a.Unavailable = true
	if got := pick("failover", session); got != "auth-b" {
		t.Fatalf("failover picked %s, want auth-b", got)
	}
	a.Unavailable = false
	a.Quota.Signals = efClaudeSignals(efWindow{"7d", 100, efHour})
	if got := pick("after recovery", session); got != "auth-b" {
		t.Fatalf("session moved back to %s after recovery, want auth-b", got)
	}
}

func TestExpiringFirstSelectorThroughManager(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		selector func() Selector
	}{
		{"plain", func() Selector { return newTestExpiringFirstSelector() }},
		{"session affinity", func() Selector { return NewSessionAffinitySelector(newTestExpiringFirstSelector()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			selector := tc.selector()
			if stoppable, ok := selector.(StoppableSelector); ok {
				defer stoppable.Stop()
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			prefix := "ef-manager-" + strings.ReplaceAll(tc.name, " ", "-") + "-"
			relaxed, urgent := prefix+"relaxed", prefix+"urgent"
			for _, auth := range []*Auth{
				efClaudeAuth(relaxed, efWindow{"7d", 90, 6 * efDay}),
				// The most urgent credential has no Fable quota left.
				efClaudeAuth(urgent, efWindow{"7d", 40, 3 * efHour}, efWindow{"7d_oi", 0, 2 * efDay}),
				// A lower priority tier never wins while the top tier is available.
				efWithPriority(efClaudeAuth(prefix+"backup", efWindow{"7d", 100, efHour}), -1),
			} {
				id := auth.ID
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}, {ID: "claude-fable-5"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("Register(%s) error = %v", id, errRegister)
				}
			}
			opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_0d8f2b6e-3c1a-4e9b-8f7d-5a2c4e6b1d93"}}`)}
			// The route model reaches the selector, so the Fable gate applies to Fable requests only.
			for _, want := range []struct{ model, id string }{
				{"", urgent},
				{"claude-opus-5-5", urgent},
				{"claude-fable-5", relaxed},
			} {
				for index := 0; index < 2; index++ {
					got, err := manager.SelectAuth(context.Background(), "claude", want.model, opts)
					if err != nil || got == nil || got.ID != want.id {
						t.Fatalf("SelectAuth(%q) #%d = %+v, %v; want %s", want.model, index, got, err, want.id)
					}
				}
			}
		})
	}
}

func TestExpiringFirstSelectorConcurrentPicks(t *testing.T) {
	t.Parallel()

	selector := newTestExpiringFirstSelector()
	auths := []*Auth{
		efClaudeAuth("t1", efWindow{"7d", 50, efDay}),
		efClaudeAuth("t2", efWindow{"7d", 50, efDay}),
		efClaudeAuth("gated", efWindow{"5h", 0, efHour}, efWindow{"7d", 90, efDay}),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for index := 0; index < 50; index++ {
				got, err := selector.Pick(context.Background(), "claude", fmt.Sprintf("claude-opus-5-%d", worker%3), cliproxyexecutor.Options{}, auths)
				if err != nil || got == nil || (got.ID != "t1" && got.ID != "t2") {
					errs <- fmt.Errorf("worker %d pick %d = %+v, %v", worker, index, got, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// efLogHook captures standard-logger entries at level for one test and
// restores the logger afterwards. Tests using it must not run in parallel.
func efLogHook(t *testing.T, level log.Level) *logtest.Hook {
	t.Helper()
	oldLevel := log.GetLevel()
	savedHooks := make(log.LevelHooks)
	for lvl, hooks := range log.StandardLogger().Hooks {
		savedHooks[lvl] = append([]log.Hook(nil), hooks...)
	}
	hook := new(logtest.Hook)
	log.AddHook(hook)
	log.SetLevel(level)
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(savedHooks)
	})
	return hook
}

func efPickEntries(hook *logtest.Hook) []*log.Entry {
	var entries []*log.Entry
	for _, entry := range hook.AllEntries() {
		if strings.HasPrefix(entry.Message, "expiring-first: pick") {
			entries = append(entries, entry)
		}
	}
	return entries
}

func TestExpiringFirstSelectorLogsPicks(t *testing.T) {
	const secret = "sk-ant-oat01-secret-token-value"
	urgent := efClaudeAuth("auth-urgent", efWindow{"7d", 40, 3 * efHour})
	urgent.Metadata = map[string]any{"access_token": secret, "refresh_token": secret}
	gated := efClaudeAuth("auth-gated", efWindow{"5h", 0, efHour}, efWindow{"7d", 90, efDay})

	t.Run("log-picks logs at info", func(t *testing.T) {
		hook := efLogHook(t, log.InfoLevel)
		selector := newTestExpiringFirstSelector()
		selector.LogPicks = true
		if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{urgent, gated}); err != nil {
			t.Fatal(err)
		}
		entries := efPickEntries(hook)
		if len(entries) != 1 || entries[0].Level != log.InfoLevel {
			t.Fatalf("pick log entries = %+v, want one info entry", entries)
		}
		entry := entries[0]
		want := log.Fields{"provider": "claude", "model": "claude-opus-5-5", "auth": "auth-urgent", "urgency": 13.333, "reason": "most-urgent"}
		for key, value := range want {
			if entry.Data[key] != value {
				t.Fatalf("field %s = %#v, want %#v (entry %+v)", key, entry.Data[key], value, entry.Data)
			}
		}
		if entry.Message != "expiring-first: pick | auth=auth-urgent urgency=13.333 window=7d" {
			t.Fatalf("message = %q", entry.Message)
		}
		if strings.Contains(fmt.Sprint(entry.Message, entry.Data), secret) {
			t.Fatal("pick log leaked a token")
		}
	})

	t.Run("all-gated pick names the gate", func(t *testing.T) {
		hook := efLogHook(t, log.InfoLevel)
		selector := newTestExpiringFirstSelector()
		selector.LogPicks = true
		if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{gated}); err != nil {
			t.Fatal(err)
		}
		entries := efPickEntries(hook)
		if len(entries) != 1 || entries[0].Data["reason"] != "all-gated" ||
			entries[0].Message != `expiring-first: pick | auth=auth-gated urgency=3.75 window=7d gate="5h exhausted" gate_resets_at=2026-10-03T22:00:00Z` {
			t.Fatalf("pick log entries = %+v", entries)
		}
	})

	t.Run("default logs at debug", func(t *testing.T) {
		hook := efLogHook(t, log.InfoLevel)
		selector := newTestExpiringFirstSelector()
		if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{efClaudeAuth("auth-unknown")}); err != nil {
			t.Fatal(err)
		}
		if entries := efPickEntries(hook); len(entries) != 0 {
			t.Fatalf("pick logged at info without log-picks: %+v", entries)
		}
		log.SetLevel(log.DebugLevel)
		if _, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{efClaudeAuth("auth-unknown")}); err != nil {
			t.Fatal(err)
		}
		entries := efPickEntries(hook)
		if len(entries) != 1 || entries[0].Level != log.DebugLevel || entries[0].Data["reason"] != "no-data" || entries[0].Data["urgency"] != "unknown" ||
			entries[0].Message != "expiring-first: pick | auth=auth-unknown urgency=unknown" {
			t.Fatalf("debug pick log entries = %+v", entries)
		}
	})
}
