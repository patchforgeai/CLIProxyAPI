package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// focusTestBody mirrors the body every /routing/focus answer returns.
type focusTestBody struct {
	Focus []struct {
		AuthID    string     `json:"auth_id"`
		AuthIndex string     `json:"auth_index"`
		Name      string     `json:"name"`
		Provider  string     `json:"provider"`
		Until     *time.Time `json:"until"`
		SetAt     time.Time  `json:"set_at"`
	} `json:"focus"`
}

// newFocusTestHandler returns a handler on its own focus store, over a pool of
// a Claude credential named by its file, one whose ID differs from its file
// name, a Codex credential, an API key without a file and a plugin virtual
// credential.
func newFocusTestHandler(t *testing.T) (*Handler, *coreauth.Manager, *routingfocus.Store) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "claude-a.json", FileName: "claude-a.json", Provider: "claude", Metadata: map[string]any{"access_token": "sk-ant-oat01-focus-secret"}},
		{ID: "claude-id-b", FileName: "claude-b.json", Provider: "claude"},
		{ID: "codex-a.json", FileName: "codex-a.json", Provider: "codex"},
		{ID: "gemini:apikey:focus", Provider: "Gemini", Attributes: map[string]string{"api_key": "AIza-focus-secret"}},
		{ID: "claude-virtual", FileName: "claude-virtual.json", Provider: "claude", Attributes: map[string]string{coreauth.AttributePluginVirtual: "true"}},
	} {
		auth.Status = coreauth.StatusActive
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	store := routingfocus.NewStore()
	h.SetRoutingFocusStore(store)
	return h, manager, store
}

// serveFocus runs one request against handler and decodes a 200 answer.
func serveFocus(t *testing.T, handler gin.HandlerFunc, method, target, body string) (int, focusTestBody, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	handler(ctx)
	var decoded focusTestBody
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		if decoded.Focus == nil {
			t.Fatalf("focus is not a list: %s", recorder.Body.String())
		}
	}
	return recorder.Code, decoded, recorder.Body.String()
}

func focusTestIDs(body focusTestBody) string {
	ids := make([]string, 0, len(body.Focus))
	for _, focus := range body.Focus {
		ids = append(ids, focus.Provider+"="+focus.AuthID)
	}
	return strings.Join(ids, ",")
}

func TestRoutingFocusEndpoints(t *testing.T) {
	h, manager, store := newFocusTestHandler(t)
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	code, body, raw := serveFocus(t, h.GetRoutingFocus, http.MethodGet, "/", "")
	if code != http.StatusOK || raw != `{"focus":[]}` {
		t.Fatalf("empty GET = %d %s", code, raw)
	}

	// By ID, without an end.
	before := time.Now().Add(-time.Second)
	code, body, raw = serveFocus(t, h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-a.json","until":null}`)
	if code != http.StatusOK || focusTestIDs(body) != "claude=claude-a.json" {
		t.Fatalf("PUT by ID = %d %s", code, raw)
	}
	auth, _ := manager.GetByID("claude-a.json")
	entry := body.Focus[0]
	if entry.Name != "claude-a.json" || entry.AuthIndex == "" || entry.AuthIndex != auth.EnsureIndex() || entry.Until != nil ||
		entry.SetAt.Before(before) || entry.SetAt.After(time.Now()) || !strings.Contains(raw, `"until":null`) {
		t.Fatalf("PUT entry = %+v (%s)", entry, raw)
	}
	if strings.Contains(raw, "secret") {
		t.Fatalf("focus body leaked a credential: %s", raw)
	}

	// By file name, with an end: it replaces the provider's focus.
	code, body, raw = serveFocus(t, h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-b.json","until":"`+until.In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339)+`"}`)
	if code != http.StatusOK || focusTestIDs(body) != "claude=claude-id-b" || body.Focus[0].Name != "claude-b.json" ||
		body.Focus[0].Until == nil || !body.Focus[0].Until.Equal(until) || !strings.Contains(raw, `"until":"`+until.Format(time.RFC3339)+`"`) {
		t.Fatalf("PUT by file name = %d %s", code, raw)
	}

	// Another provider keeps its own focus; a credential without a file is named by its ID.
	if code, body, raw = serveFocus(t, h.PutRoutingFocus, http.MethodPut, "/", `{"name":"codex-a.json"}`); code != http.StatusOK {
		t.Fatalf("PUT codex = %d %s", code, raw)
	}
	if code, body, raw = serveFocus(t, h.PutRoutingFocus, http.MethodPut, "/", `{"name":" gemini:apikey:focus "}`); code != http.StatusOK {
		t.Fatalf("PUT gemini = %d %s", code, raw)
	}
	if focusTestIDs(body) != "claude=claude-id-b,codex=codex-a.json,gemini=gemini:apikey:focus" || body.Focus[2].Name != "gemini:apikey:focus" {
		t.Fatalf("after three providers = %s", raw)
	}
	if code, body, raw = serveFocus(t, h.GetRoutingFocus, http.MethodGet, "/", ""); code != http.StatusOK ||
		focusTestIDs(body) != "claude=claude-id-b,codex=codex-a.json,gemini=gemini:apikey:focus" {
		t.Fatalf("GET = %d %s", code, raw)
	}
	if !store.Focused("claude-id-b", time.Now()) || store.Focused("claude-a.json", time.Now()) {
		t.Fatal("the store does not hold what the endpoint reports")
	}

	// DELETE by provider, then by file name; both are idempotent.
	for _, step := range []struct{ target, want string }{
		{"/?provider=Codex", "claude=claude-id-b,gemini=gemini:apikey:focus"},
		{"/?provider=codex", "claude=claude-id-b,gemini=gemini:apikey:focus"},
		{"/?name=claude-b.json", "gemini=gemini:apikey:focus"},
		{"/?name=claude-id-b", "gemini=gemini:apikey:focus"},
		{"/?name=unknown.json", "gemini=gemini:apikey:focus"},
		{"/?name=gemini:apikey:focus", ""},
	} {
		if code, body, raw = serveFocus(t, h.DeleteRoutingFocus, http.MethodDelete, step.target, ""); code != http.StatusOK || focusTestIDs(body) != step.want {
			t.Fatalf("DELETE %s = %d %s, want %q", step.target, code, raw, step.want)
		}
	}
}

func TestRoutingFocusRefusals(t *testing.T) {
	h, _, store := newFocusTestHandler(t)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	tests := []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		target  string
		body    string
		want    int
		wantErr string
	}{
		{"unknown credential", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"missing.json"}`, http.StatusNotFound, "auth file not found"},
		{"plugin virtual credential", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-virtual.json"}`, http.StatusConflict, errPluginVirtualAuth.Error()},
		{"empty body", h.PutRoutingFocus, http.MethodPut, "/", ``, http.StatusBadRequest, "invalid request body"},
		{"not JSON", h.PutRoutingFocus, http.MethodPut, "/", `{"name":`, http.StatusBadRequest, "invalid request body"},
		{"name not a string", h.PutRoutingFocus, http.MethodPut, "/", `{"name":7}`, http.StatusBadRequest, "invalid request body"},
		{"no name", h.PutRoutingFocus, http.MethodPut, "/", `{"until":null}`, http.StatusBadRequest, "name is required"},
		{"blank name", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"  "}`, http.StatusBadRequest, "name is required"},
		{"until not a string", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-a.json","until":1760000000}`, http.StatusBadRequest, "invalid request body"},
		{"until not RFC 3339", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-a.json","until":"tomorrow"}`, http.StatusBadRequest, "until must be an RFC 3339 time or null"},
		{"until without a zone", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-a.json","until":"2099-01-01T00:00:00"}`, http.StatusBadRequest, "until must be an RFC 3339 time or null"},
		{"until in the past", h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-a.json","until":"` + past + `"}`, http.StatusBadRequest, "until must be in the future"},
		{"delete without a selector", h.DeleteRoutingFocus, http.MethodDelete, "/", ``, http.StatusBadRequest, "exactly one of name and provider is required"},
		{"delete with both", h.DeleteRoutingFocus, http.MethodDelete, "/?name=claude-a.json&provider=claude", ``, http.StatusBadRequest, "exactly one of name and provider is required"},
		{"delete with a blank name", h.DeleteRoutingFocus, http.MethodDelete, "/?name=%20", ``, http.StatusBadRequest, "exactly one of name and provider is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := serveFocus(t, tc.handler, tc.method, tc.target, tc.body)
			var answer struct {
				Error string `json:"error"`
			}
			if errDecode := json.Unmarshal([]byte(raw), &answer); code != tc.want || errDecode != nil || answer.Error != tc.wantErr {
				t.Fatalf("status = %d body = %s, want %d {\"error\":%q}", code, raw, tc.want, tc.wantErr)
			}
			if !store.Empty() {
				t.Fatalf("a refused request changed the focus: %+v", store.List(time.Now()))
			}
		})
	}
}

func TestRoutingFocusListsOnlyExistingCredentials(t *testing.T) {
	h, _, store := newFocusTestHandler(t)
	for _, focus := range []routingfocus.Focus{
		{AuthID: "removed-claude.json", Provider: "claude"},
		{AuthID: "codex-a.json", Provider: "codex"},
	} {
		if err := store.Set(focus); err != nil {
			t.Fatal(err)
		}
	}
	if code, body, raw := serveFocus(t, h.GetRoutingFocus, http.MethodGet, "/", ""); code != http.StatusOK || focusTestIDs(body) != "codex=codex-a.json" {
		t.Fatalf("GET = %d %s, want only the existing credential", code, raw)
	}
}

func TestRoutingFocusPersistsThroughTheStoreFile(t *testing.T) {
	h, _, _ := newFocusTestHandler(t)
	path := filepath.Join(t.TempDir(), routingfocus.FileName)
	store := routingfocus.NewStore()
	if err := store.Load(path); err != nil {
		t.Fatal(err)
	}
	h.SetRoutingFocusStore(store)
	if code, _, raw := serveFocus(t, h.PutRoutingFocus, http.MethodPut, "/", `{"name":"claude-a.json"}`); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, raw)
	}
	reloaded := routingfocus.NewStore()
	if err := reloaded.Load(path); err != nil || !reloaded.Focused("claude-a.json", time.Now()) {
		t.Fatalf("the focus did not survive a reload: %v", err)
	}
}

// The quota observer forgets the focus of a removed credential with its readings.
func TestQuotaObserverForgetsRemovedCredentialsFocus(t *testing.T) {
	h, manager, store := newFocusTestHandler(t)
	for _, focus := range []routingfocus.Focus{
		{AuthID: "claude-a.json", Provider: "claude"},
		{AuthID: "codex-a.json", Provider: "codex"},
	} {
		if err := store.Set(focus); err != nil {
			t.Fatal(err)
		}
	}
	h.observeQuotaOnce(context.Background())
	if got := len(store.List(time.Now())); got != 2 {
		t.Fatalf("observer dropped the focus of existing credentials: %d left", got)
	}
	if _, err := manager.Update(context.Background(), &coreauth.Auth{ID: "codex-a.json", FileName: "codex-a.json", Provider: "codex", Disabled: true, Status: coreauth.StatusDisabled}); err != nil {
		t.Fatal(err)
	}
	h.observeQuotaOnce(context.Background())
	if got := len(store.List(time.Now())); got != 2 {
		t.Fatalf("observer dropped the focus of a disabled credential: %d left", got)
	}
	manager.Remove(context.Background(), "codex-a.json")
	h.observeQuotaOnce(context.Background())
	if store.Focused("codex-a.json", time.Now()) || !store.Focused("claude-a.json", time.Now()) {
		t.Fatalf("after removing codex-a.json: %+v", store.List(time.Now()))
	}
}

func TestForgetRemovedFocus(t *testing.T) {
	t.Parallel()

	listedAt := time.Now()
	auths := []*coreauth.Auth{{ID: "kept.json"}, nil}
	for _, tc := range []struct {
		name  string
		auths []*coreauth.Auth
		setAt time.Time
		want  string
	}{
		{"a removed credential loses its focus", auths, listedAt.Add(-time.Second), "kept.json"},
		{"a focus newer than the list stays", auths, listedAt.Add(time.Second), "kept.json,gone.json"},
		{"an empty list removes nothing", nil, listedAt.Add(-time.Second), "kept.json,gone.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := routingfocus.NewStore()
			for _, focus := range []routingfocus.Focus{
				{AuthID: "kept.json", Provider: "claude", SetAt: listedAt.Add(-time.Hour)},
				{AuthID: "gone.json", Provider: "codex", SetAt: tc.setAt},
			} {
				if err := store.Set(focus); err != nil {
					t.Fatal(err)
				}
			}
			forgetRemovedFocus(store, tc.auths, listedAt)
			var ids []string
			for _, focus := range store.List(time.Now()) {
				ids = append(ids, focus.AuthID)
			}
			if got := strings.Join(ids, ","); got != tc.want {
				t.Fatalf("focused = %s, want %s", got, tc.want)
			}
		})
	}
}
