package codexaccountmodels

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// completeEntry is a complete Codex client entry for slug: the embedded client
// catalog's first entry, renamed.
func completeEntry(t *testing.T, slug string) map[string]any {
	t.Helper()
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(registry.GetCodexClientModelsJSON(), &payload); err != nil || len(payload.Models) == 0 {
		t.Fatalf("embedded codex client catalog: %v", err)
	}
	data, _ := json.Marshal(payload.Models[0])
	var entry map[string]any
	_ = json.Unmarshal(data, &entry)
	entry["slug"] = slug
	entry["display_name"] = "Display " + slug
	return entry
}

func TestParseCatalog(t *testing.T) {
	entries, err := ParseCatalog([]byte(`{"models":[{"slug":"gpt-a","context_window":500000},{"display_name":"no slug"},{"slug":" "},{"slug":"gpt-b"},{"slug":"gpt-a"}]}`))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if got := (Learned{Entries: entries}).Slugs(); !reflect.DeepEqual(got, []string{"gpt-a", "gpt-b"}) {
		t.Fatalf("slugs = %v, want the named models once each, in order", got)
	}
	if _, ok := entries[0]["context_window"].(float64); !ok {
		t.Fatalf("numbers decode as float64, as the client catalog's templates do: %T", entries[0]["context_window"])
	}
	for _, body := range []string{``, `not json`, `{}`, `{"models":null}`, `[]`} {
		if _, err := ParseCatalog([]byte(body)); err == nil {
			t.Fatalf("ParseCatalog(%q) succeeded, want an error", body)
		}
	}
	if entries, err := ParseCatalog([]byte(`{"models":[]}`)); err != nil || len(entries) != 0 {
		t.Fatalf("an empty catalog is an answer: %v %v", entries, err)
	}
}

func TestModelInfo_KnownModelIsTheCatalogs(t *testing.T) {
	known := registry.GetCodexProModels()
	if len(known) == 0 {
		t.Fatal("no Codex catalog models")
	}
	id := known[0].ID
	info := ModelInfo(map[string]any{"slug": id, "display_name": "Something else"})
	if info == nil || info.ID != id || info.DisplayName == "Something else" {
		t.Fatalf("ModelInfo(%s) = %+v, want the catalog's own description", id, info)
	}
}

func TestModelInfo_UnknownModelFromOpenAIsEntry(t *testing.T) {
	info := ModelInfo(map[string]any{
		"slug":           "gpt-from-openai-only",
		"display_name":   "GPT From OpenAI",
		"description":    "A model the catalog lacks.",
		"context_window": float64(500000),
		"input_modalities": []any{
			"text", "image",
		},
		"supported_reasoning_levels": []any{
			map[string]any{"effort": "low"},
			map[string]any{"effort": "High"},
			map[string]any{"effort": "ultra"},
			map[string]any{"description": "no effort"},
		},
	})
	if info == nil {
		t.Fatal("no model info")
	}
	if info.ID != "gpt-from-openai-only" || info.OwnedBy != "openai" || info.Type != "openai" || info.Object != "model" {
		t.Fatalf("identity = %+v", info)
	}
	if info.DisplayName != "GPT From OpenAI" || info.Description != "A model the catalog lacks." || info.ContextLength != 500000 {
		t.Fatalf("description = %+v", info)
	}
	if !reflect.DeepEqual(info.SupportedInputModalities, []string{"text", "image"}) {
		t.Fatalf("input modalities = %v", info.SupportedInputModalities)
	}
	// "ultra" is Codex's orchestration mode, which the catalog leaves out too.
	if info.Thinking == nil || !reflect.DeepEqual(info.Thinking.Levels, []string{"low", "high"}) {
		t.Fatalf("thinking = %+v, want low and high", info.Thinking)
	}

	bare := ModelInfo(map[string]any{"slug": "gpt-bare"})
	if bare.DisplayName != "gpt-bare" || !reflect.DeepEqual(bare.SupportedInputModalities, []string{"text"}) || bare.Thinking != nil {
		t.Fatalf("bare = %+v", bare)
	}
	if ModelInfo(map[string]any{"display_name": "x"}) != nil {
		t.Fatal("an entry without a slug describes no model")
	}
}

func TestStoreSetReportsChanges(t *testing.T) {
	store := NewStore()
	first := Learned{ClientVersion: "0.163.0", At: time.Unix(1, 0), Entries: []map[string]any{{"slug": "gpt-a"}}}
	if !store.Set("auth-1", first) {
		t.Fatal("a first answer is a change")
	}
	again := first
	again.At = time.Unix(2, 0)
	if store.Set("auth-1", again) {
		t.Fatal("the same entries answered later are not a change")
	}
	if got, _ := store.Get("auth-1"); !got.At.Equal(time.Unix(2, 0)) {
		t.Fatal("the later answer is kept")
	}
	if !store.Set("auth-1", Learned{ClientVersion: "0.163.0", Entries: []map[string]any{{"slug": "gpt-a"}, {"slug": "gpt-b"}}}) {
		t.Fatal("a new model is a change")
	}
	if store.Set(" ", first) {
		t.Fatal("a blank credential is refused")
	}
	store.Set("auth-2", first)
	store.Retain(map[string]struct{}{"auth-2": {}})
	if _, ok := store.Get("auth-1"); ok {
		t.Fatal("a removed credential is forgotten")
	}
	if _, ok := store.Get("auth-2"); !ok {
		t.Fatal("a kept credential stays")
	}
}

func TestStoreTemplates(t *testing.T) {
	store := NewStore()
	older := completeEntry(t, "gpt-shared")
	older["display_name"] = "older"
	newer := completeEntry(t, "gpt-shared")
	newer["display_name"] = "newer"
	store.Set("auth-a", Learned{ClientVersion: "0.162.0", At: time.Unix(5, 0), Entries: []map[string]any{older, {"slug": "gpt-incomplete"}}})
	store.Set("auth-b", Learned{ClientVersion: "0.163.0", At: time.Unix(1, 0), Entries: []map[string]any{newer, completeEntry(t, "gpt-only-b")}})

	templates := store.Templates()
	if got := templates["gpt-shared"]["display_name"]; got != "newer" {
		t.Fatalf("shared model's template = %v, want the newest client version's", got)
	}
	if _, ok := templates["gpt-only-b"]; !ok {
		t.Fatal("every account's models have templates")
	}
	if _, ok := templates["gpt-incomplete"]; ok {
		t.Fatal("an entry the client catalog would refuse is no template")
	}
	if NewStore().Templates() != nil {
		t.Fatal("an empty store has no templates")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.163.0", "0.163.0", 0},
		{"0.163.0", "0.162.9", 1},
		{"0.99.0", "0.163.0", -1},
		{"1.0", "0.999.0", 1},
		{"v0.163.0", "0.163.0", 0},
		{"0.164.0-alpha.2", "0.164.0", -1},
		{"0.164.0-alpha.10", "0.164.0-alpha.2", -1},
		{"0.164.0", "", 1},
		{"garbage", "0.1.0", -1},
		{"", "", 0},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, valid := range []string{"0.163.0", "1.2", "0.164.0-alpha.3"} {
		if !ValidVersion(valid) {
			t.Fatalf("%q is a version", valid)
		}
	}
	for _, invalid := range []string{"", "1", "a.b.c", "1.2.3.4.5", "-1.0.0"} {
		if ValidVersion(invalid) {
			t.Fatalf("%q is not a version", invalid)
		}
	}
}

type recordingLearner struct {
	ensured  []string
	resolved []string
}

func (r *recordingLearner) Ensure(_ context.Context, clientVersion string) {
	r.ensured = append(r.ensured, clientVersion)
}

func (r *recordingLearner) Resolve(_ context.Context, model string) bool {
	r.resolved = append(r.resolved, model)
	return true
}

func TestLearnerHook(t *testing.T) {
	t.Cleanup(func() { SetLearner(nil) })
	SetLearner(nil)
	Ensure(context.Background(), "0.163.0")
	if Resolve(context.Background(), "gpt-x") {
		t.Fatal("without a learner nothing is resolved")
	}
	learner := &recordingLearner{}
	SetLearner(learner)
	Ensure(context.Background(), "0.163.0")
	if !Resolve(context.Background(), "gpt-x") {
		t.Fatal("the learner's answer is returned")
	}
	if !reflect.DeepEqual(learner.ensured, []string{"0.163.0"}) || !reflect.DeepEqual(learner.resolved, []string{"gpt-x"}) {
		t.Fatalf("learner saw ensured=%v resolved=%v", learner.ensured, learner.resolved)
	}
}
