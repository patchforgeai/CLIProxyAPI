package cliproxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudeaccountmodels"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// fakeClaudeModels is a learner's world: credentials, a clock, and Anthropic.
type fakeClaudeModels struct {
	mu          sync.Mutex
	now         time.Time
	credentials []*coreauth.Auth
	// pages by credential ID and after_id: a page body.
	pages map[string]map[string]string
	fail  map[string]error
	// gate, when set, holds every lookup until it is closed.
	gate       chan struct{}
	fetched    []string // "<auth>@<after_id>"
	registered []string
}

func newFakeClaudeModels(ids ...string) *fakeClaudeModels {
	f := &fakeClaudeModels{now: time.Unix(1_800_000_000, 0), pages: map[string]map[string]string{}, fail: map[string]error{}}
	for _, id := range ids {
		f.credentials = append(f.credentials, &coreauth.Auth{ID: id, Provider: "claude"})
		f.pages[id] = map[string]string{"": `{"data":[{"id":"claude-fable-5-2"},{"id":"claude-opus-5-5"}],"has_more":false}`}
	}
	return f
}

func (f *fakeClaudeModels) learner() *devyreClaudeModelLearner {
	l := newDevyreClaudeModelLearner(nil)
	l.store = claudeaccountmodels.NewStore()
	l.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	l.credentials = func() []*coreauth.Auth {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]*coreauth.Auth(nil), f.credentials...)
	}
	l.fetch = func(ctx context.Context, auth *coreauth.Auth, afterID string) ([]byte, error) {
		f.mu.Lock()
		gate := f.gate
		f.fetched = append(f.fetched, auth.ID+"@"+afterID)
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := f.fail[auth.ID]; err != nil {
			return nil, err
		}
		return []byte(f.pages[auth.ID][afterID]), nil
	}
	l.register = func(auth *coreauth.Auth) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.registered = append(f.registered, auth.ID)
	}
	return l
}

func (f *fakeClaudeModels) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeClaudeModels) snapshot() (fetched, registered []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...), append([]string(nil), f.registered...)
}

func idleClaude(t *testing.T, l *devyreClaudeModelLearner) {
	t.Helper()
	l.mu.Lock()
	done := l.running
	l.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup did not end")
	}
}

func TestDevyreClaudeModels_EnsureLearnsBeforeTheListIsAnswered(t *testing.T) {
	f := newFakeClaudeModels("claude-a.json", "claude-b.json")
	l := f.learner()

	l.Ensure(context.Background())
	fetched, registered := f.snapshot()
	if len(fetched) != 2 || len(registered) != 2 {
		t.Fatalf("fetched=%v registered=%v, want both credentials asked and registered before Ensure returned", fetched, registered)
	}
	learned, ok := l.store.Get("claude-a.json")
	if !ok || strings.Join(learned.IDs(), ",") != "claude-fable-5-2,claude-opus-5-5" {
		t.Fatalf("learned = %+v", learned)
	}

	// Answered and fresh: nothing is asked again.
	l.Ensure(context.Background())
	if fetched, _ := f.snapshot(); len(fetched) != 2 {
		t.Fatalf("fetched = %v, want no new lookup", fetched)
	}
}

func TestDevyreClaudeModels_ReadsEveryPage(t *testing.T) {
	f := newFakeClaudeModels("claude-a.json")
	f.pages["claude-a.json"] = map[string]string{
		"":                 `{"data":[{"id":"claude-fable-5-2"}],"has_more":true,"last_id":"claude-fable-5-2"}`,
		"claude-fable-5-2": `{"data":[{"id":"claude-opus-5-5"}],"has_more":false,"last_id":"claude-opus-5-5"}`,
	}
	l := f.learner()
	l.Ensure(context.Background())
	fetched, _ := f.snapshot()
	if strings.Join(fetched, ",") != "claude-a.json@,claude-a.json@claude-fable-5-2" {
		t.Fatalf("fetched = %v, want both pages", fetched)
	}
	if learned, _ := l.store.Get("claude-a.json"); strings.Join(learned.IDs(), ",") != "claude-fable-5-2,claude-opus-5-5" {
		t.Fatalf("learned = %v", learned.IDs())
	}
}

func TestDevyreClaudeModels_StaleAnswersRefreshBehindTheList(t *testing.T) {
	f := newFakeClaudeModels("claude-a.json")
	l := f.learner()
	l.Ensure(context.Background())

	f.advance(devyreClaudeModelsTTL)
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.pages["claude-a.json"][""] = `{"data":[{"id":"claude-fable-5-2"},{"id":"claude-opus-6"}],"has_more":false}`
	f.mu.Unlock()

	returned := make(chan struct{})
	go func() {
		l.Ensure(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a stale answer must not hold the model list")
	}
	close(gate)
	idleClaude(t, l)
	learned, _ := l.store.Get("claude-a.json")
	if strings.Join(learned.IDs(), ",") != "claude-fable-5-2,claude-opus-6" {
		t.Fatalf("learned = %v, want the refreshed answer", learned.IDs())
	}
	if _, registered := f.snapshot(); len(registered) != 2 {
		t.Fatalf("registered = %v, want the changed credential registered again", registered)
	}
}

func TestDevyreClaudeModels_AFailedLookupKeepsWhatItHad(t *testing.T) {
	f := newFakeClaudeModels("claude-a.json", "claude-b.json")
	l := f.learner()
	l.Ensure(context.Background())

	f.advance(devyreClaudeModelsTTL)
	f.mu.Lock()
	f.fail["claude-a.json"] = errors.New("the list answered 401")
	f.pages["claude-b.json"][""] = `not json`
	f.mu.Unlock()
	l.Ensure(context.Background())
	idleClaude(t, l)
	for _, id := range []string{"claude-a.json", "claude-b.json"} {
		if learned, ok := l.store.Get(id); !ok || len(learned.Entries) != 2 {
			t.Fatalf("%s learned = %+v, want the earlier answer kept", id, learned)
		}
	}

	// A credential that never answered is not waited for again within the gap.
	g := newFakeClaudeModels("claude-c.json")
	g.fail["claude-c.json"] = errors.New("down")
	lc := g.learner()
	lc.Ensure(context.Background())
	before, _ := g.snapshot()
	lc.Ensure(context.Background())
	if after, _ := g.snapshot(); len(after) != len(before) {
		t.Fatalf("fetched %v after %v, want no lookup while the failure is recent", after, before)
	}
	g.advance(devyreClaudeModelsRetryGap)
	g.mu.Lock()
	delete(g.fail, "claude-c.json")
	g.mu.Unlock()
	lc.Ensure(context.Background())
	if learned, ok := lc.store.Get("claude-c.json"); !ok || len(learned.Entries) != 2 {
		t.Fatalf("learned = %+v, want the answer once the gap passed", learned)
	}
}

func TestDevyreClaudeModels_ResolveAnUnknownModel(t *testing.T) {
	if newFakeClaudeModels().learner().Resolve(context.Background(), "claude-fable-5-2") {
		t.Fatal("without a Claude account there is nothing to ask")
	}
	f := newFakeClaudeModels("claude-a.json")
	l := f.learner()
	if l.Resolve(context.Background(), " ") {
		t.Fatal("no model, nothing to resolve")
	}
	if !l.Resolve(context.Background(), "claude-fable-5-2") {
		t.Fatal("the first unknown model starts a lookup and waits for it")
	}
	if l.Resolve(context.Background(), "claude-nobody-serves") {
		t.Fatal("within the gap an unknown model starts no lookup")
	}
	f.advance(devyreClaudeModelsRetryGap)
	if !l.Resolve(context.Background(), "claude-nobody-serves") {
		t.Fatal("after the gap it asks again")
	}
	if fetched, _ := f.snapshot(); len(fetched) != 2 {
		t.Fatalf("fetched = %v", fetched)
	}
}

func TestDevyreClaudeModels_OneLookupAtATime(t *testing.T) {
	f := newFakeClaudeModels("claude-a.json")
	gate := make(chan struct{})
	f.gate = gate
	l := f.learner()

	var wg sync.WaitGroup
	results := make([]bool, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 0 {
				l.Ensure(context.Background())
				results[i] = true
				return
			}
			results[i] = l.Resolve(context.Background(), "claude-fable-5-2")
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fetched, _ := f.snapshot()
		if len(fetched) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fetched = %v, want exactly one lookup", fetched)
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	if fetched, _ := f.snapshot(); len(fetched) != 1 {
		t.Fatalf("fetched = %v, want the callers to share one lookup", fetched)
	}
	for i, ok := range results {
		if !ok {
			t.Fatalf("caller %d did not see the lookup end", i)
		}
	}
}

func TestDevyreClaudeModels_RemovedCredentialsAreForgotten(t *testing.T) {
	f := newFakeClaudeModels("claude-a.json", "claude-b.json")
	l := f.learner()
	l.Ensure(context.Background())
	f.mu.Lock()
	f.credentials = f.credentials[:1]
	f.mu.Unlock()
	f.advance(devyreClaudeModelsTTL)
	l.Ensure(context.Background())
	idleClaude(t, l)
	if _, ok := l.store.Get("claude-b.json"); ok {
		t.Fatal("a removed credential's answer is dropped")
	}
}

func TestDevyreAsksAnthropicForModels(t *testing.T) {
	for _, tc := range []struct {
		auth *coreauth.Auth
		want bool
	}{
		{&coreauth.Auth{ID: "sub.json", Provider: "claude"}, true},
		{&coreauth.Auth{ID: "key", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": "https://api.anthropic.com"}}, true},
		{&coreauth.Auth{ID: "gateway", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": "https://gateway.example"}}, false},
		{&coreauth.Auth{ID: "off.json", Provider: "claude", Disabled: true}, false},
		{&coreauth.Auth{ID: "codex.json", Provider: "codex"}, false},
		{nil, false},
	} {
		if got := devyreAsksAnthropicForModels(tc.auth); got != tc.want {
			t.Fatalf("%+v: got %v, want %v", tc.auth, got, tc.want)
		}
	}
}

func TestDevyreWithAccountClaudeModels(t *testing.T) {
	store := claudeaccountmodels.Default()
	t.Cleanup(func() { store.Retain(nil) })
	catalog := []*ModelInfo{
		{ID: "claude-opus-5-5", DisplayName: "catalog's"},
		{ID: "claude-haiku-4-5-20251001", DisplayName: "a dated name"},
	}
	if got := devyreWithAccountClaudeModels("claude-none.json", catalog); len(got) != 2 {
		t.Fatalf("without an answer the catalog stands: %v", got)
	}
	store.Set("claude-a.json", claudeaccountmodels.Learned{Entries: []claudeaccountmodels.Entry{
		{ID: "CLAUDE-OPUS-5-5", DisplayName: "Anthropic's"},
		{ID: "claude-devyre-fable-9-9", DisplayName: "Claude Devyre Fable 9.9"},
	}})
	got := devyreWithAccountClaudeModels("claude-a.json", catalog)
	var ids []string
	for _, model := range got {
		ids = append(ids, model.ID)
	}
	if strings.Join(ids, ",") != "claude-opus-5-5,claude-haiku-4-5-20251001,claude-devyre-fable-9-9" || got[0].DisplayName != "catalog's" {
		t.Fatalf("models = %v, want the catalog kept whole and the missing one added", ids)
	}
}

// Through the real registration: a model only Anthropic lists for the account
// is served by the account, routing finds it, and the Claude model list shows
// it with Anthropic's name and context window.
func TestRegisterModelsForAuth_ClaudeAddsTheAccountsOwnModels(t *testing.T) {
	const model = "claude-devyre-account-only-9-9"
	store := claudeaccountmodels.Default()
	t.Cleanup(func() { store.Retain(nil) })
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:         "claude-devyre-account.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "token"},
	}
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)
	if providers := util.GetProviderName(model); len(providers) != 0 {
		t.Fatalf("providers = %v before Anthropic was asked", providers)
	}
	before := len(registry.GetModelsForClient(auth.ID))

	store.Set(auth.ID, claudeaccountmodels.Learned{Entries: []claudeaccountmodels.Entry{{
		ID: model, DisplayName: "Claude Account Only 9.9", MaxInputTokens: 1_000_000, MaxTokens: 128_000,
	}}})
	service.registerModelsForAuth(context.Background(), auth)
	providers := util.GetProviderName(model)
	if len(providers) != 1 || providers[0] != "claude" {
		t.Fatalf("providers = %v, want claude", providers)
	}
	served := registry.GetModelsForClient(auth.ID)
	if len(served) != before+1 {
		t.Fatalf("served %d models, want the catalog's %d and the account's own", len(served), before)
	}
	found := false
	for _, entry := range registry.GetAvailableModels("claude") {
		if entry["id"] == model {
			found = true
			if entry["display_name"] != "Claude Account Only 9.9" {
				t.Fatalf("entry = %v", entry)
			}
		}
	}
	if !found {
		t.Fatal("the Claude model list does not show the account's own model")
	}
}
