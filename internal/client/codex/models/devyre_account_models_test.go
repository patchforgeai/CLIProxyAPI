package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// openAIEntry is a complete Codex client entry as OpenAI lists one: the
// embedded catalog's first entry under another slug and name.
func openAIEntry(t *testing.T, slug, displayName string) map[string]any {
	t.Helper()
	var payload codexClientModelsPayload
	if err := json.Unmarshal(registry.GetCodexClientModelsJSON(), &payload); err != nil || len(payload.Models) == 0 {
		t.Fatalf("embedded codex client catalog: %v", err)
	}
	entry := cloneCodexClientModelMap(payload.Models[0])
	entry["slug"] = slug
	entry["display_name"] = displayName
	entry["description"] = "OpenAI's own description of " + slug + "."
	return entry
}

func TestCodexClientModels_AccountModelsUseOpenAIsEntry(t *testing.T) {
	const slug = "gpt-devyre-openai-only"
	store := codexaccountmodels.Default()
	t.Cleanup(func() { store.Retain(nil) })

	before := BuildResponseForClient([]map[string]any{{"id": slug}}, nil, false, "0.163.0")
	beforeModels := before["models"].([]map[string]any)
	if len(beforeModels) != 1 || beforeModels[0]["description"] == "OpenAI's own description of "+slug+"." {
		t.Fatalf("before OpenAI was asked, the generic stand-in describes it: %+v", beforeModels)
	}

	store.Set("codex-a.json", codexaccountmodels.Learned{
		ClientVersion: "0.163.0",
		At:            time.Unix(1, 0),
		Entries: []map[string]any{
			openAIEntry(t, slug, "GPT Devyre OpenAI Only"),
			// The catalog's own template wins over the account's entry.
			openAIEntry(t, "gpt-5.5", "Not the catalog's"),
		},
	})
	resp := BuildResponseForClient([]map[string]any{{"id": slug}, {"id": "gpt-5.5"}}, nil, false, "0.163.0")
	models := resp["models"].([]map[string]any)
	bySlug := map[string]map[string]any{}
	for _, model := range models {
		bySlug[stringModelValue(model, "slug")] = model
	}
	if got := bySlug[slug]["description"]; got != "OpenAI's own description of "+slug+"." {
		t.Fatalf("description = %v, want OpenAI's entry as the template", got)
	}
	if got := bySlug["gpt-5.5"]["display_name"]; got == "Not the catalog's" {
		t.Fatal("the catalog's template must win")
	}

	// The cached templates were never written.
	templates, _, err := loadCodexClientModelTemplatesSnapshot(registry.GetCodexClientModelsSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := templates[slug]; ok {
		t.Fatal("the account's entry leaked into the cached catalog")
	}
}
