package handlers

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// resolvingAntigravityLearner registers the name it is asked to resolve, as
// the service's learner does once Google lists it for an Antigravity account.
type resolvingAntigravityLearner struct {
	clientID string
	asked    []string
}

func (l *resolvingAntigravityLearner) Ensure(context.Context) {}

func (l *resolvingAntigravityLearner) Resolve(_ context.Context, model string) bool {
	l.asked = append(l.asked, model)
	registry.GetGlobalRegistry().RegisterClient(l.clientID, "antigravity", []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: "antigravity", Type: "antigravity"}})
	return true
}

func TestGetRequestDetails_AsksTheAntigravityAccountsBeforeRefusing(t *testing.T) {
	const model = "gemini-devyre-learned-on-demand"
	learner := &resolvingAntigravityLearner{clientID: "ag-devyre-on-demand.json"}
	antigravityaccountmodels.SetLearner(learner)
	t.Cleanup(func() {
		antigravityaccountmodels.SetLearner(nil)
		registry.GetGlobalRegistry().UnregisterClient(learner.clientID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	providers, normalized, errMsg := handler.getRequestDetails(model + "(low)")
	if errMsg != nil {
		t.Fatalf("errMsg = %v, want the name routed once the accounts were asked", errMsg.Error)
	}
	if len(providers) != 1 || providers[0] != "antigravity" || normalized != model+"(low)" {
		t.Fatalf("providers=%v model=%q", providers, normalized)
	}
	if len(learner.asked) != 1 || learner.asked[0] != model {
		t.Fatalf("asked = %v, want the name without its suffix", learner.asked)
	}

	// A model the registry knows never waits on the accounts.
	learner.asked = nil
	if _, _, errMsg := handler.getRequestDetails(model); errMsg != nil || len(learner.asked) != 0 {
		t.Fatalf("errMsg=%v asked=%v", errMsg, learner.asked)
	}
}
