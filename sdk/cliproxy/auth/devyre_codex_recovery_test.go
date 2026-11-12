package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ChatGPT's refusal of a request for the account's usage limit, as the Codex
// executor hands it on: the body is the error's message.
const codexUsageLimitBody = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":86400}}`

var codexRecoveryModels = []string{"gpt-6.1-sol", "gpt-6.1-terra", "gpt-5.3-codex-spark"}

func codexRecoveryManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.RegisterExecutor(schedulerProviderTestExecutor{provider: "codex"})
	id := uuid.NewString()
	if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	models := make([]*registry.ModelInfo, 0, len(codexRecoveryModels))
	for _, model := range codexRecoveryModels {
		models = append(models, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	return m, id
}

// rejectCodexUsageLimit fails a request for model on the account's usage limit,
// which blocks the whole credential until the reset the rejection named, wait
// from now.
func rejectCodexUsageLimit(m *Manager, id, model string, wait time.Duration) {
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: model, CredentialScope: true,
		RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: codexUsageLimitBody}})
}

// reconcileCodexUsage reconciles a fresh usage response that reports the
// account at none of its limits, whose call started at started.
func reconcileCodexUsage(m *Manager, id string, started time.Time) {
	observed, _ := m.GetByID(id)
	m.ReconcileCodexUsage(observed, true, started)
}

func TestCodexUsageRecoveryMakesRequestsSelectable(t *testing.T) {
	m, id := codexRecoveryManager(t)
	ctx := context.Background()
	store := &recordingCooldownStateStore{}
	m.SwapCooldownStateStore(ctx, store, false)
	// A model already used before the rejection receives its propagated block.
	m.MarkResult(ctx, Result{AuthID: id, Provider: "codex", Model: "gpt-6.1-terra", Success: true})
	rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 24*time.Hour)
	for _, model := range codexRecoveryModels {
		if _, err := m.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err == nil {
			t.Fatalf("the account at its usage limit still served %s", model)
		}
	}
	reconcileCodexUsage(m, id, time.Now().Add(time.Second))
	if records := store.savedRecords(); len(records) != 0 {
		t.Fatalf("recovery left persisted cooldowns: %+v", records)
	}
	for _, model := range codexRecoveryModels {
		if _, err := m.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("recovered account could not serve %s: %v", model, err)
		}
	}
}

// The requests in flight as an account runs out are each rejected, and each
// rejection writes the credential-wide block over the other models' states.
func TestCodexUsageRecoveryAfterSeveralModelsWereRejected(t *testing.T) {
	for _, order := range []string{"later deadline last", "earlier deadline last"} {
		t.Run(order, func(t *testing.T) {
			m, id := codexRecoveryManager(t)
			first, last := 24*time.Hour, 24*time.Hour+20*time.Second
			if order == "earlier deadline last" {
				first, last = last, first
			}
			rejectCodexUsageLimit(m, id, "gpt-6.1-sol", first)
			time.Sleep(2 * time.Millisecond)
			rejectCodexUsageLimit(m, id, "gpt-6.1-terra", last)
			reconcileCodexUsage(m, id, time.Now().Add(time.Second))
			for _, model := range codexRecoveryModels {
				if blockedForModel(m, id, model) {
					t.Errorf("%s is still blocked after the usage showed the account at none of its limits", model)
				}
			}
		})
	}
}

func TestCodexUsageRecoveryNeedsNewEvidence(t *testing.T) {
	for _, scenario := range []string{"older", "same instant", "raced by a new rejection", "rejection over", "disabled", "auth error", "billing error", "permission error"} {
		t.Run(scenario, func(t *testing.T) {
			m, id := codexRecoveryManager(t)
			rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 24*time.Hour)
			observed, _ := m.GetByID(id)
			started := observed.UpdatedAt.Add(time.Second)
			switch scenario {
			case "older":
				started = observed.UpdatedAt.Add(-time.Second)
			case "same instant":
				started = observed.UpdatedAt
			case "raced by a new rejection":
				// Even on the same clock tick, the generation tells them apart.
				rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 24*time.Hour)
			case "rejection over":
				// The model's own rejection ran out: it explains no block that still holds.
				m.mu.Lock()
				m.auths[id].ModelStates["gpt-6.1-sol"].Quota.NextRecoverAt = time.Now().Add(-time.Minute)
				m.mu.Unlock()
				observed, _ = m.GetByID(id)
			case "disabled":
				m.mu.Lock()
				m.auths[id].Disabled = true
				m.mu.Unlock()
				observed, _ = m.GetByID(id)
			default:
				status := map[string]int{"auth error": 401, "billing error": 402, "permission error": 403}[scenario]
				m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: "gpt-6.1-sol", Error: &Error{HTTPStatus: status}})
				observed, _ = m.GetByID(id)
				started = time.Now().Add(time.Second)
			}
			m.ReconcileCodexUsage(observed, true, started)
			if !blockedForModel(m, id, "gpt-6.1-sol") {
				t.Fatal("insufficient recovery evidence unblocked the account")
			}
		})
	}
}

// A usage response that still shows a limit reached lifts nothing.
func TestCodexUsageRecoveryNeedsRoom(t *testing.T) {
	m, id := codexRecoveryManager(t)
	rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 24*time.Hour)
	observed, _ := m.GetByID(id)
	m.ReconcileCodexUsage(observed, false, time.Now().Add(time.Second))
	if !blockedForModel(m, id, "gpt-6.1-sol") {
		t.Fatal("a usage response without room unblocked the account")
	}
}

// A 429 that is no usage limit says nothing the account's usage could
// contradict: a per-minute rate limit, a model at capacity.
func TestCodexUsageRecoveryLeavesOtherRateLimitsAlone(t *testing.T) {
	wait := 10 * time.Minute
	for name, body := range map[string]string{
		"rate limit":        `{"error":{"type":"rate_limit_exceeded","message":"Rate limit reached"}}`,
		"model at capacity": `{"error":{"message":"Selected model is at capacity. Please try a different model."}}`,
		"not JSON":          `usage_limit_reached`,
	} {
		t.Run(name, func(t *testing.T) {
			m, id := codexRecoveryManager(t)
			m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: "gpt-6.1-sol",
				RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: body}})
			reconcileCodexUsage(m, id, time.Now().Add(time.Second))
			if !blockedForModel(m, id, "gpt-6.1-sol") {
				t.Fatal("usage lifted a rate limit it says nothing of")
			}
		})
	}
}

// A model rejected with the others and then again by an ordinary limit keeps
// that later block. The account and the other model come back.
func TestCodexUsageRecoveryKeepsALaterOrdinaryLimitAmongSeveralModels(t *testing.T) {
	m, id := codexRecoveryManager(t)
	rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 24*time.Hour)
	rejectCodexUsageLimit(m, id, "gpt-6.1-terra", 24*time.Hour+20*time.Second)
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: "gpt-6.1-terra",
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"type":"rate_limit_exceeded"}}`}})
	reconcileCodexUsage(m, id, time.Now().Add(time.Second))
	if blockedForModel(m, id, "gpt-6.1-sol") {
		t.Fatal("the recovered model is still blocked")
	}
	if !blockedForModel(m, id, "gpt-6.1-terra") {
		t.Fatal("usage cleared a later ordinary rate limit")
	}
}

// With upstream.codex.model-level-cooling, a usage limit cools the requested
// model alone: its cooldown is lifted the same way, and nothing else changes.
func TestCodexUsageRecoveryOfAModelScopedUsageLimit(t *testing.T) {
	m, id := codexRecoveryManager(t)
	wait := 24 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: "gpt-6.1-sol",
		RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: codexUsageLimitBody}})
	if !blockedForModel(m, id, "gpt-6.1-sol") || blockedForModel(m, id, "gpt-6.1-terra") {
		t.Fatal("precondition: the usage limit cools the requested model alone")
	}
	reconcileCodexUsage(m, id, time.Now().Add(time.Second))
	if blockedForModel(m, id, "gpt-6.1-sol") {
		t.Fatal("the model is still blocked after the usage showed the account at none of its limits")
	}
}

// Claude's recovery reads Claude's usage: neither lifts the other provider's hold.
func TestUsageRecoveryIsItsProvidersOwn(t *testing.T) {
	codex, codexID := codexRecoveryManager(t)
	rejectCodexUsageLimit(codex, codexID, "gpt-6.1-sol", 24*time.Hour)
	observed, _ := codex.GetByID(codexID)
	codex.ReconcileClaudeUsage(observed, recoveredUsage(), nil, time.Now().Add(time.Second))
	if !blockedForModel(codex, codexID, "gpt-6.1-sol") {
		t.Fatal("a Claude usage response lifted a ChatGPT account's hold")
	}

	claude, claudeID := recoveryManager(t)
	rejectSubscription(claude, claudeID, "claude-opus-5-5", true)
	observed, _ = claude.GetByID(claudeID)
	claude.ReconcileCodexUsage(observed, true, time.Now().Add(time.Second))
	if !blockedForModel(claude, claudeID, "claude-opus-5-5") {
		t.Fatal("a ChatGPT usage response lifted a Claude account's hold")
	}
}
