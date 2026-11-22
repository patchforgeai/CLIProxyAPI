package gemini

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// ensuringLearner registers a Gemini API name when the model list asks, as the
// service's learner does once Google answered for an Antigravity account.
type ensuringLearner struct{ clientID string }

func (l *ensuringLearner) Ensure(context.Context) {
	registry.GetGlobalRegistry().RegisterClient(l.clientID, "antigravity", []*registry.ModelInfo{{
		ID: "gemini-devyre-learned", Name: "gemini-devyre-learned", DisplayName: "Gemini Devyre Learned", Type: "antigravity",
	}})
}

func (l *ensuringLearner) Resolve(context.Context, string) bool { return false }

func TestGeminiModels_AsksTheAntigravityAccountsFirst(t *testing.T) {
	learner := &ensuringLearner{clientID: "ag-devyre-models-list.json"}
	antigravityaccountmodels.SetLearner(learner)
	t.Cleanup(func() {
		antigravityaccountmodels.SetLearner(nil)
		registry.GetGlobalRegistry().UnregisterClient(learner.clientID)
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/v1beta/models", nil)
	NewGeminiAPIHandler(&handlers.BaseAPIHandler{}).GeminiModels(ctx)

	var response struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	for _, model := range response.Models {
		if model.Name == "models/gemini-devyre-learned" && model.DisplayName == "Gemini Devyre Learned" {
			return
		}
	}
	t.Fatal("the list was answered before the Antigravity accounts were asked")
}
