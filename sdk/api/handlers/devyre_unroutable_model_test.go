package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestGetRequestDetails_UnknownModelIsTyped(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	_, _, errMsg := handler.getRequestDetails("gpt-devyre-nobody-serves")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("errMsg = %+v, want a 400", errMsg)
	}
	if !IsUnroutableModel(errMsg.Error) {
		t.Fatalf("error %T is not the router's own unroutable model answer", errMsg.Error)
	}
	if !IsUnroutableModel(fmt.Errorf("wrapped: %w", errMsg.Error)) {
		t.Fatal("a wrapped answer is still recognised")
	}
	if IsUnroutableModel(errors.New(errMsg.Error.Error())) {
		t.Fatal("an upstream's model_not_found body is not the router's own answer")
	}
	var status interface{ StatusCode() int }
	if !errors.As(errMsg.Error, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatal("the answer carries its status")
	}
}

// resolvingLearner registers the model it is asked to resolve, as the service's
// learner does once OpenAI lists it for a Codex account.
type resolvingLearner struct {
	clientID string
	asked    []string
}

func (l *resolvingLearner) Ensure(context.Context, string) {}

func (l *resolvingLearner) Resolve(_ context.Context, model string) bool {
	l.asked = append(l.asked, model)
	registry.GetGlobalRegistry().RegisterClient(l.clientID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: "openai", Type: "openai"}})
	return true
}

func TestGetRequestDetails_AsksTheCodexAccountsBeforeRefusing(t *testing.T) {
	const model = "gpt-devyre-learned-on-demand"
	learner := &resolvingLearner{clientID: "codex-devyre-on-demand.json"}
	codexaccountmodels.SetLearner(learner)
	t.Cleanup(func() {
		codexaccountmodels.SetLearner(nil)
		registry.GetGlobalRegistry().UnregisterClient(learner.clientID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	providers, normalized, errMsg := handler.getRequestDetails(model)
	if errMsg != nil {
		t.Fatalf("errMsg = %v, want the model routed once the accounts were asked", errMsg.Error)
	}
	if len(providers) != 1 || providers[0] != "codex" || normalized != model {
		t.Fatalf("providers=%v model=%q", providers, normalized)
	}
	if len(learner.asked) != 1 || learner.asked[0] != model {
		t.Fatalf("asked = %v", learner.asked)
	}

	// A model the registry knows never waits on the accounts.
	learner.asked = nil
	if _, _, errMsg := handler.getRequestDetails(model); errMsg != nil || len(learner.asked) != 0 {
		t.Fatalf("errMsg=%v asked=%v", errMsg, learner.asked)
	}
}
