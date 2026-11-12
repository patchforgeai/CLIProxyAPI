package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func recoveryManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.RegisterExecutor(schedulerProviderTestExecutor{provider: "claude"})
	id := uuid.NewString()
	_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{
		{ID: "claude-opus-5-5"}, {ID: "claude-sonnet-5-5"}, {ID: "claude-fable-5-1"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	return m, id
}

func rejectSubscription(m *Manager, id, model string, shared bool) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	window := "7d"
	if !shared {
		window = "7d_oi"
	}
	logging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-" + window + "-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-" + window + "-Reset":  {strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)},
	})
	delay := 24 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: model, CredentialScope: shared,
		RetryAfter: &delay, Error: &Error{HTTPStatus: 429, Message: "subscription window rejected"}})
}

func recoveredUsage() []quotareading.Window {
	return []quotareading.Window{
		{ID: "5h", Kind: quotareading.KindShort, UsedPercent: 0},
		{ID: "7d", Kind: quotareading.KindLong, UsedPercent: 0},
	}
}

func reconcileUsage(m *Manager, id string, windows []quotareading.Window, started time.Time) {
	reconcileUsageWithout(m, id, windows, nil, started)
}

// reconcileUsageWithout reconciles a usage response that reports the shared
// windows in unlimited as limits the account does not have.
func reconcileUsageWithout(m *Manager, id string, windows []quotareading.Window, unlimited []string, started time.Time) {
	observed, _ := m.GetByID(id)
	m.ReconcileClaudeUsage(observed, windows, unlimited, started)
}

// rejectSharedWindow fails a request for model on a rejected shared window, as
// Anthropic answers one once a subscription window is spent: the headers name
// the window, and the cooldown runs to its reset plus the fuzz each rejection
// draws for itself.
func rejectSharedWindow(m *Manager, id, model, window string, fuzz time.Duration) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	reset := time.Now().Add(24 * time.Hour)
	logging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-Status":                {"rejected"},
		"Anthropic-Ratelimit-Unified-" + window + "-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-" + window + "-Reset":  {strconv.FormatInt(reset.Unix(), 10)},
	})
	delay := 24*time.Hour + fuzz
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: model, CredentialScope: true,
		RetryAfter: &delay, Error: &Error{HTTPStatus: 429, Message: "subscription window rejected"}})
}

func blockedForModel(m *Manager, id, model string) bool {
	auth, _ := m.GetByID(id)
	blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now())
	return blocked
}

func TestClaudeUsageRecoveryPreservesRejectionDuringRefresh(t *testing.T) {
	m, id := recoveryManager(t)
	rejectSubscription(m, id, "claude-opus-5-5", true)
	observed, _ := m.GetByID(id)
	// Even if both results share a clock tick, the generation distinguishes them.
	rejectSubscription(m, id, "claude-opus-5-5", true)
	m.ReconcileClaudeUsage(observed, recoveredUsage(), nil, time.Now().Add(time.Second))
	after, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(after, "claude-opus-5-5", time.Now()); !blocked {
		t.Fatal("a response racing a new rejection cleared it")
	}
}

func TestClaudeUsageRecoveryMakesRequestsSelectable(t *testing.T) {
	m, id := recoveryManager(t)
	ctx := context.Background()
	store := &recordingCooldownStateStore{}
	m.SwapCooldownStateStore(ctx, store, false)
	request := cliproxyexecutor.Request{Model: "claude-opus-5-5"}
	// A model already used before the rejection receives its propagated block.
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-5-5", Success: true})
	rejectSubscription(m, id, request.Model, true)
	if _, err := m.Execute(ctx, []string{"claude"}, request, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("the rejected subscription still served requests")
	}
	reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
	if records := store.savedRecords(); len(records) != 0 {
		t.Fatalf("recovery left persisted cooldowns: %+v", records)
	}
	for _, model := range []string{request.Model, "claude-sonnet-5-5"} {
		request.Model = model
		if _, err := m.Execute(ctx, []string{"claude"}, request, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("recovered account could not serve %s: %v", model, err)
		}
	}
}

func TestClaudeUsageRecoveryDoesNotReuseOldRejectionHeaders(t *testing.T) {
	m, id := recoveryManager(t)
	rejectSubscription(m, id, "claude-opus-5-5", true)
	// A later per-minute rejection has no subscription headers of its own.
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", Error: &Error{HTTPStatus: 429, Message: "requests per minute"}})
	reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
	after, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(after, "claude-opus-5-5", time.Now()); !blocked {
		t.Fatal("old subscription headers cleared a later unrelated rate limit")
	}
}

func TestClaudeUsageRecoveryAfterConcurrentSuccess(t *testing.T) {
	m, id := recoveryManager(t)
	rejectSubscription(m, id, "claude-opus-5-5", true)
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-5-5", Success: true})
	reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
	after, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(after, "claude-opus-5-5", time.Now()); blocked {
		t.Fatal("an unrelated success hid the subscription's recovery")
	}
}

func TestClaudeUsageRecoveryNeedsNewCompleteEvidence(t *testing.T) {
	for _, scenario := range []string{"older", "same instant", "missing weekly", "missing 5-hour", "no window listed", "still exhausted", "ordinary rate limit", "rejection over", "disabled", "auth error", "billing error", "permission error"} {
		t.Run(scenario, func(t *testing.T) {
			m, id := recoveryManager(t)
			rejectSubscription(m, id, "claude-opus-5-5", true)
			auth, _ := m.GetByID(id)
			started := auth.UpdatedAt.Add(time.Second)
			windows := recoveredUsage()
			var unlimited []string
			switch scenario {
			case "older":
				started = auth.UpdatedAt.Add(-time.Second)
			case "same instant":
				started = auth.UpdatedAt
			case "missing weekly":
				windows = windows[:1]
			case "missing 5-hour":
				// Left out, not reported as a limit the account does not have.
				windows = windows[1:]
			case "no window listed":
				windows, unlimited = nil, []string{"5h", "7d"}
			case "rejection over":
				// The model's own rejection ran out: it explains no block that still holds.
				m.mu.Lock()
				m.auths[id].ModelStates["claude-opus-5-5"].Quota.NextRecoverAt = time.Now().Add(-time.Minute)
				m.mu.Unlock()
			case "still exhausted":
				windows[1].UsedPercent = 100
			case "ordinary rate limit":
				m.mu.Lock()
				m.auths[id].Quota.Signals = nil
				m.auths[id].ModelStates["claude-opus-5-5"].Quota.Signals = nil
				m.mu.Unlock()
			case "disabled":
				m.mu.Lock()
				m.auths[id].Disabled = true
				m.mu.Unlock()
			default:
				status := map[string]int{"auth error": 401, "billing error": 402, "permission error": 403}[scenario]
				m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", Error: &Error{HTTPStatus: status}})
			}
			reconcileUsageWithout(m, id, windows, unlimited, started)
			after, _ := m.GetByID(id)
			if blocked, _, _ := isAuthBlockedForModel(after, "claude-opus-5-5", time.Now()); !blocked {
				t.Fatal("insufficient recovery evidence unblocked the account")
			}
		})
	}
}

func TestClaudeUsageRecoveryPreservesOtherModelFailures(t *testing.T) {
	for _, status := range []int{404, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			m, id := recoveryManager(t)
			m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-5-5", Error: &Error{HTTPStatus: status, Message: "independent failure"}})
			rejectSubscription(m, id, "claude-opus-5-5", true)
			reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
			after, _ := m.GetByID(id)
			if blocked, _, _ := isAuthBlockedForModel(after, "claude-opus-5-5", time.Now()); blocked {
				t.Fatal("the recovered model is still blocked")
			}
			if blocked, _, _ := isAuthBlockedForModel(after, "claude-sonnet-5-5", time.Now()); !blocked {
				t.Fatal("usage cleared an unrelated model failure")
			}
		})
	}
}

func TestClaudeUsageRecoveryRequiresTheRejectedScopedWindow(t *testing.T) {
	m, id := recoveryManager(t)
	rejectSubscription(m, id, "claude-fable-5-1", false)
	started := time.Now().Add(time.Second)
	windows := recoveredUsage()
	reconcileUsage(m, id, windows, started)
	after, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(after, "claude-fable-5-1", time.Now()); !blocked {
		t.Fatal("shared usage cleared an unreported model limit")
	}
	windows = append(windows, quotareading.Window{ID: "7d:fable", Kind: quotareading.KindScoped, Model: "fable", UsedPercent: 0})
	reconcileUsage(m, id, windows, started)
	after, _ = m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(after, "claude-fable-5-1", time.Now()); blocked {
		t.Fatal("recovered scoped window did not lift its limit")
	}
}

// A plan without a 5-hour limit reports five_hour as null: its usage lists the
// weekly window alone. Requiring both windows to be listed kept such an account
// blocked until its old reset time, whatever its usage said.
func TestClaudeUsageRecoveryOfAnAccountWithoutAShortWindow(t *testing.T) {
	weekly := []quotareading.Window{{ID: "7d", Kind: quotareading.KindLong, UsedPercent: 0}}
	for _, window := range []string{"7d", "5h"} {
		t.Run(window+" rejected", func(t *testing.T) {
			m, id := recoveryManager(t)
			// Rejected on the weekly window, or on the 5-hour one before the plan lost that limit.
			rejectSharedWindow(m, id, "claude-opus-5-5", window, 0)
			reconcileUsageWithout(m, id, weekly, []string{"5h"}, time.Now().Add(time.Second))
			if blockedForModel(m, id, "claude-opus-5-5") {
				t.Fatal("an account without a 5-hour limit stayed blocked after its usage read as started over")
			}
		})
	}
}

// When an account runs out, every request in flight on it is rejected, often
// for more than one model. Each rejection writes the credential-wide block
// over the other models' states and keeps the later deadline (each draws its
// own fuzz), so the model whose state still reads "quota" need not hold the
// block's deadline, and the others read "credential_quota" with an error of
// their own. Recovery used to find no evidence then, or left the models
// rejected first blocked.
func TestClaudeUsageRecoveryAfterSeveralModelsWereRejected(t *testing.T) {
	for _, order := range []string{"later deadline last", "earlier deadline last"} {
		t.Run(order, func(t *testing.T) {
			m, id := recoveryManager(t)
			ctx := context.Background()
			// A model that only ever succeeded receives the block as propagated.
			m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-fable-5-1", Success: true})
			first, last := 5*time.Second, 25*time.Second
			if order == "earlier deadline last" {
				first, last = last, first
			}
			rejectSharedWindow(m, id, "claude-opus-5-5", "7d", first)
			time.Sleep(2 * time.Millisecond)
			rejectSharedWindow(m, id, "claude-sonnet-5-5", "7d", last)
			models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"}
			for _, model := range models {
				if !blockedForModel(m, id, model) {
					t.Fatalf("precondition: %s is not blocked", model)
				}
			}
			reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
			for _, model := range models {
				if _, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
					t.Errorf("recovered account could not serve %s: %v", model, err)
				}
			}
		})
	}
}

// A model rejected with the others and then again by an ordinary limit keeps
// that later block: the usage says nothing of a per-minute limit. The account
// and the other model come back.
func TestClaudeUsageRecoveryKeepsALaterOrdinaryLimitAmongSeveralModels(t *testing.T) {
	m, id := recoveryManager(t)
	rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 5*time.Second)
	rejectSharedWindow(m, id, "claude-sonnet-5-5", "7d", 25*time.Second)
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-5-5", Error: &Error{HTTPStatus: 429, Message: "requests per minute"}})
	reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
	if blockedForModel(m, id, "claude-opus-5-5") {
		t.Fatal("the recovered model is still blocked")
	}
	if !blockedForModel(m, id, "claude-sonnet-5-5") {
		t.Fatal("usage cleared a later ordinary rate limit")
	}
}
