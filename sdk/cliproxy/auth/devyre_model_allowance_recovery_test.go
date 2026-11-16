package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// rejectFableAllowance fails a Fable request on Fable's own weekly allowance as Anthropic's 429s
// did on 2026-11-16: the claim seven_day_overage_included, Fable's window and overage rejected,
// the shared windows not reported. Builds before the fork read the claim held the whole credential
// for it (CredentialScope), which is what restored or long-lived holds still look like.
func rejectFableAllowance(m *Manager, id string, until time.Duration) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	reset := strconv.FormatInt(time.Now().Add(until).Unix(), 10)
	logging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
		"Anthropic-Ratelimit-Unified-Reset":                {reset},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":         {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":          {reset},
		"Anthropic-Ratelimit-Unified-Overage-Status":       {"rejected"},
	})
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-fable-5-1", CredentialScope: true,
		RetryAfter: &until, Error: &Error{HTTPStatus: 429, Message: "This request would exceed your account's rate limit."}})
}

// usageWithFable is a fresh usage response with room in both shared windows and Fable's own window
// at fablePercent.
func usageWithFable(fablePercent float64) []quotareading.Window {
	return append(recoveredUsage(),
		quotareading.Window{ID: "7d:fable", Kind: quotareading.KindScoped, Model: "fable", UsedPercent: fablePercent})
}

// The hold that stopped Zephyrus's every Claude session on 2026-11-16: a Fable-only rejection
// held the whole credential, and a usage reading with room in both shared windows lifted Fable's
// own hold at most and left the credential-wide one, which nothing else lifted before its deadline
// days later ("its usage contradicts no rejection on record"). The shared windows' room is what
// that hold stood for, so the reading lifts it: every other model is served again at once, and
// Fable stays held while its own window is spent.
func TestClaudeUsageLiftsACredentialHeldOnAModelsOwnAllowance(t *testing.T) {
	t.Run("Fable's window still spent", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		rejectFableAllowance(m, id, 137*time.Hour)
		for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
			if !blockedForModel(m, id, model) {
				t.Fatalf("precondition: %s is not held", model)
			}
		}
		reconcileUsage(m, id, usageWithFable(100), time.Now().Add(time.Second))
		for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5"} {
			if _, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
				t.Errorf("the account could not serve %s once its shared windows showed room: %v", model, err)
			}
		}
		if !blockedForModel(m, id, "claude-fable-5-1") {
			t.Fatal("Fable was served while its own window is spent")
		}
		expectParts(t, oneLine(t, lines, "quota: back in use", id), "provider=claude", "by=usage")
	})
	t.Run("a reset started Fable's window over too", func(t *testing.T) {
		m, id := recoveryManager(t)
		rejectFableAllowance(m, id, 137*time.Hour)
		reconcileUsage(m, id, usageWithFable(0), time.Now().Add(time.Second))
		for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
			if _, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
				t.Errorf("the account could not serve %s after its limits started over: %v", model, err)
			}
		}
	})
	t.Run("a shared window spent", func(t *testing.T) {
		m, id := recoveryManager(t)
		rejectFableAllowance(m, id, 137*time.Hour)
		spent := []quotareading.Window{
			{ID: "5h", Kind: quotareading.KindShort, UsedPercent: 100},
			{ID: "7d", Kind: quotareading.KindLong, UsedPercent: 10},
			{ID: "7d:fable", Kind: quotareading.KindScoped, Model: "fable", UsedPercent: 0},
		}
		reconcileUsage(m, id, spent, time.Now().Add(time.Second))
		if !blockedForModel(m, id, "claude-opus-5-5") {
			t.Fatal("a usage reading with the 5-hour window spent lifted the credential's hold")
		}
	})
}

// A rejection the fork cannot tie to any window (only the overall status says "rejected") stays
// no evidence of anything: the usage says nothing of it.
func TestClaudeUsageKeepsAHoldNoWindowExplains(t *testing.T) {
	m, id := recoveryManager(t)
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}})
	wait := 24 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", CredentialScope: true,
		RetryAfter: &wait, Error: &Error{HTTPStatus: 429, Message: "rejected"}})
	reconcileUsage(m, id, usageWithFable(0), time.Now().Add(time.Second))
	if !blockedForModel(m, id, "claude-sonnet-5-5") {
		t.Fatal("a usage reading lifted a hold no window explains")
	}
}

// "back in use" is said only once the credential takes requests again. When a usage reading ends
// some models' own holds while a credential-wide block it does not contradict still stands, the
// log names those models and why the credential is still held: on 2026-11-16 it said the account
// was back in use, and it took no request.
func TestQuotaLogSaysBackInUseOnlyOnceTheCredentialIs(t *testing.T) {
	lines := captureLog(t)
	m, id := recoveryManager(t)
	rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
	observed, _ := m.GetByID(id)
	m.reconcileUsage(observed, "claude", true, time.Now().Add(time.Second), func(auth *Auth, now time.Time) quotaLift {
		resetModelState(auth.ModelStates["claude-opus-5-5"], now)
		return quotaLift{models: []string{"claude-opus-5-5"}}
	})
	if back := lines.of("quota: back in use", id); len(back) != 0 {
		t.Fatalf("a credential still held was said to be back in use: %q", back)
	}
	expectParts(t, oneLine(t, lines, "quota: models back in use", id), "models=claude-opus-5-5", "by=usage")
	expectParts(t, oneLine(t, lines, "quota: hold kept", id), `why="its usage contradicts no rejection on record"`)
}
