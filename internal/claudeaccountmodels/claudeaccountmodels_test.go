package claudeaccountmodels

import (
	"strings"
	"testing"
	"time"
)

// A page as Anthropic's GET /v1/models answers it (2026-11-09), trimmed.
const fablePage = `{
  "data": [
    {
      "type": "model",
      "id": "claude-devyre-fable-9-9",
      "display_name": "Claude Devyre Fable 9.9",
      "created_at": "2026-11-09T00:00:00Z",
      "max_input_tokens": 1000000,
      "max_tokens": 128000,
      "capabilities": {
        "image_input": {"supported": true},
        "thinking": {"supported": true, "types": {"enabled": {"supported": false}, "adaptive": {"supported": true}}},
        "effort": {"supported": true, "low": {"supported": true}, "medium": {"supported": true}, "high": {"supported": true}, "xhigh": {"supported": true}, "max": {"supported": true}}
      }
    },
    {"type": "model", "id": "claude-haiku-4-5-20251001", "display_name": "Claude Haiku 4.5", "max_input_tokens": 200000, "max_tokens": 64000,
     "capabilities": {"image_input": {"supported": true}, "effort": {"supported": false}}},
    {"type": "model", "id": "not-a-claude-model"},
    {"type": "model", "id": "  "}
  ],
  "has_more": true,
  "first_id": "claude-devyre-fable-9-9",
  "last_id": "claude-haiku-4-5-20251001"
}`

func TestParsePage(t *testing.T) {
	page, err := ParsePage([]byte(fablePage))
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.LastID != "claude-haiku-4-5-20251001" {
		t.Fatalf("page = %+v", page)
	}
	if got := strings.Join((Learned{Entries: page.Entries}).IDs(), ","); got != "claude-devyre-fable-9-9,claude-haiku-4-5-20251001" {
		t.Fatalf("ids = %s, want only the Claude models", got)
	}
	for _, body := range []string{`not json`, `{"models":[]}`} {
		if _, err := ParsePage([]byte(body)); err == nil {
			t.Fatalf("%s parsed", body)
		}
	}
}

func TestMergeKeepsTheFirstOfEachID(t *testing.T) {
	first := Page{Entries: []Entry{{ID: "claude-a-1", DisplayName: "first"}, {ID: "claude-b-1"}}}
	second := Page{Entries: []Entry{{ID: "CLAUDE-A-1", DisplayName: "again"}, {ID: "claude-c-1"}}}
	merged := Merge(first, second)
	if got := strings.Join((Learned{Entries: merged}).IDs(), ","); got != "claude-a-1,claude-b-1,claude-c-1" || merged[0].DisplayName != "first" {
		t.Fatalf("merged = %+v", merged)
	}
}

// A model the catalog does not know is described by Anthropic's entry: its
// name, limits, image input and effort levels.
func TestModelInfoFromAnthropicsEntry(t *testing.T) {
	page, err := ParsePage([]byte(fablePage))
	if err != nil {
		t.Fatal(err)
	}
	info := ModelInfo(page.Entries[0])
	if info == nil {
		t.Fatal("no info")
	}
	if info.ID != "claude-devyre-fable-9-9" || info.DisplayName != "Claude Devyre Fable 9.9" || info.Type != "claude" || info.OwnedBy != "anthropic" {
		t.Fatalf("info = %+v", info)
	}
	if info.ContextLength != 1_000_000 || info.MaxCompletionTokens != 128_000 {
		t.Fatalf("limits = %d/%d", info.ContextLength, info.MaxCompletionTokens)
	}
	if info.Created != time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("created = %d", info.Created)
	}
	if strings.Join(info.SupportedInputModalities, ",") != "text,image" {
		t.Fatalf("inputs = %v", info.SupportedInputModalities)
	}
	if info.Thinking == nil || !info.Thinking.DynamicAllowed || strings.Join(info.Thinking.Levels, ",") != "low,medium,high,xhigh,max" {
		t.Fatalf("thinking = %+v", info.Thinking)
	}

	// No effort support: no levels; no name: the id.
	bare := ModelInfo(Entry{ID: "claude-devyre-bare-1", Capabilities: map[string]any{"effort": map[string]any{"supported": false}}})
	if bare.Thinking != nil || bare.DisplayName != "claude-devyre-bare-1" || strings.Join(bare.SupportedInputModalities, ",") != "text" {
		t.Fatalf("bare = %+v", bare)
	}
	if ModelInfo(Entry{}) != nil {
		t.Fatal("an entry without an id describes nothing")
	}
}

func TestStore(t *testing.T) {
	store := NewStore()
	a := Learned{At: time.Unix(1, 0), Entries: []Entry{{ID: "claude-a-1"}}}
	if !store.Set("a.json", a) {
		t.Fatal("the first answer is a change")
	}
	if store.Set("a.json", Learned{At: time.Unix(2, 0), Entries: []Entry{{ID: "claude-a-1"}}}) {
		t.Fatal("the same models at a later time are no change")
	}
	if got, ok := store.Get(" a.json "); !ok || !got.At.Equal(time.Unix(2, 0)) {
		t.Fatalf("got = %+v", got)
	}
	store.Retain(map[string]struct{}{})
	if _, ok := store.Get("a.json"); ok {
		t.Fatal("a removed credential is forgotten")
	}
}
