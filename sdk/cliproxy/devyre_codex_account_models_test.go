package cliproxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexaccountmodels"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// fakeCodexModels is a learner's world: credentials, a clock, and OpenAI.
type fakeCodexModels struct {
	mu          sync.Mutex
	now         time.Time
	credentials []*coreauth.Auth
	// answers by credential ID: a catalog body, or an error.
	answers map[string]string
	fail    map[string]error
	// gate, when set, holds every lookup until it is closed.
	gate       chan struct{}
	fetched    []string // "<auth>@<version>"
	registered []string
}

func newFakeCodexModels(ids ...string) *fakeCodexModels {
	f := &fakeCodexModels{now: time.Unix(1_800_000_000, 0), answers: map[string]string{}, fail: map[string]error{}}
	for _, id := range ids {
		f.credentials = append(f.credentials, &coreauth.Auth{ID: id, Provider: "codex"})
		f.answers[id] = `{"models":[{"slug":"gpt-6.1-terra"},{"slug":"gpt-6.1-sol"}]}`
	}
	return f
}

func (f *fakeCodexModels) learner() *devyreCodexModelLearner {
	l := newDevyreCodexModelLearner(nil)
	l.store = codexaccountmodels.NewStore()
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
	l.fetch = func(ctx context.Context, auth *coreauth.Auth, version string) ([]byte, error) {
		f.mu.Lock()
		gate := f.gate
		f.fetched = append(f.fetched, auth.ID+"@"+version)
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
		return []byte(f.answers[auth.ID]), nil
	}
	l.register = func(auth *coreauth.Auth) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.registered = append(f.registered, auth.ID)
	}
	return l
}

func (f *fakeCodexModels) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeCodexModels) snapshot() (fetched, registered []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...), append([]string(nil), f.registered...)
}

// idle waits until no lookup runs (the goroutine that ends one closes its
// channel after clearing it).
func idle(t *testing.T, l *devyreCodexModelLearner) {
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

func TestDevyreCodexModels_EnsureLearnsBeforeTheListIsAnswered(t *testing.T) {
	f := newFakeCodexModels("codex-a.json", "codex-b.json")
	l := f.learner()

	l.Ensure(context.Background(), "0.163.0")
	fetched, registered := f.snapshot()
	if len(fetched) != 2 || len(registered) != 2 {
		t.Fatalf("fetched=%v registered=%v, want both credentials asked and registered before Ensure returned", fetched, registered)
	}
	learned, ok := l.store.Get("codex-a.json")
	if !ok || learned.ClientVersion != "0.163.0" || strings.Join(learned.Slugs(), ",") != "gpt-6.1-terra,gpt-6.1-sol" {
		t.Fatalf("learned = %+v", learned)
	}

	// Answered for this version and fresh: nothing is asked again.
	l.Ensure(context.Background(), "0.163.0")
	l.Ensure(context.Background(), "")
	l.Ensure(context.Background(), "0.150.0")
	if fetched, _ := f.snapshot(); len(fetched) != 2 {
		t.Fatalf("fetched = %v, want no new lookup", fetched)
	}

	// A newer client is answered for its own version.
	l.Ensure(context.Background(), "0.164.0")
	fetched, registered = f.snapshot()
	if len(fetched) != 4 || !strings.HasSuffix(fetched[3], "@0.164.0") {
		t.Fatalf("fetched = %v, want both asked for 0.164.0", fetched)
	}
	if len(registered) != 2 {
		t.Fatalf("registered = %v: the same models are no change", registered)
	}
	// An older client does not lower the version asked for.
	f.advance(devyreCodexModelsTTL)
	l.Ensure(context.Background(), "0.150.0")
	idle(t, l)
	fetched, _ = f.snapshot()
	if len(fetched) != 6 || !strings.HasSuffix(fetched[5], "@0.164.0") {
		t.Fatalf("fetched = %v, want the stale answers refreshed for 0.164.0", fetched)
	}
}

func TestDevyreCodexModels_StaleAnswersRefreshBehindTheList(t *testing.T) {
	f := newFakeCodexModels("codex-a.json")
	l := f.learner()
	l.Ensure(context.Background(), "0.163.0")

	f.advance(devyreCodexModelsTTL)
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.answers["codex-a.json"] = `{"models":[{"slug":"gpt-6.1-terra"},{"slug":"gpt-7-new"}]}`
	f.mu.Unlock()

	returned := make(chan struct{})
	go func() {
		l.Ensure(context.Background(), "0.163.0")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a stale answer must not hold the model list")
	}
	close(gate)
	idle(t, l)
	learned, _ := l.store.Get("codex-a.json")
	if strings.Join(learned.Slugs(), ",") != "gpt-6.1-terra,gpt-7-new" {
		t.Fatalf("learned = %v, want the refreshed answer", learned.Slugs())
	}
	if _, registered := f.snapshot(); len(registered) != 2 {
		t.Fatalf("registered = %v, want the changed credential registered again", registered)
	}
}

func TestDevyreCodexModels_AFailedLookupKeepsWhatItHad(t *testing.T) {
	f := newFakeCodexModels("codex-a.json", "codex-b.json")
	l := f.learner()
	l.Ensure(context.Background(), "0.163.0")

	f.mu.Lock()
	f.fail["codex-a.json"] = errors.New("the catalog answered 401")
	f.answers["codex-b.json"] = `not json`
	f.mu.Unlock()
	l.Ensure(context.Background(), "0.164.0")
	for _, id := range []string{"codex-a.json", "codex-b.json"} {
		learned, ok := l.store.Get(id)
		if !ok || learned.ClientVersion != "0.163.0" || len(learned.Entries) != 2 {
			t.Fatalf("%s learned = %+v, want the earlier answer kept", id, learned)
		}
	}

	// A credential that just failed is not waited for again at once.
	before, _ := f.snapshot()
	l.Ensure(context.Background(), "0.164.0")
	if after, _ := f.snapshot(); len(after) != len(before) {
		t.Fatalf("fetched %v after %v, want no lookup while the failures are recent", after, before)
	}
	f.advance(devyreCodexModelsRetryGap)
	f.mu.Lock()
	delete(f.fail, "codex-a.json")
	f.answers["codex-b.json"] = `{"models":[{"slug":"gpt-6.1-terra"}]}`
	f.mu.Unlock()
	l.Ensure(context.Background(), "0.164.0")
	if learned, _ := l.store.Get("codex-b.json"); learned.ClientVersion != "0.164.0" || len(learned.Entries) != 1 {
		t.Fatalf("learned = %+v, want the new answer once the gap passed", learned)
	}
}

func TestDevyreCodexModels_ResolveAnUnknownModel(t *testing.T) {
	none := newFakeCodexModels()
	if none.learner().Resolve(context.Background(), "gpt-7-new") {
		t.Fatal("without a Codex account there is nothing to ask")
	}

	f := newFakeCodexModels("codex-a.json")
	l := f.learner()
	if l.Resolve(context.Background(), " ") {
		t.Fatal("no model, nothing to resolve")
	}
	if !l.Resolve(context.Background(), "gpt-6.1-terra") {
		t.Fatal("the first unknown model starts a lookup and waits for it")
	}
	if fetched, _ := f.snapshot(); len(fetched) != 1 || fetched[0] != "codex-a.json@"+codexaccountmodels.FloorClientVersion {
		t.Fatalf("fetched = %v, want one lookup for the floor version", fetched)
	}
	if l.Resolve(context.Background(), "gpt-nobody-serves") {
		t.Fatal("within the gap an unknown model starts no lookup")
	}
	f.advance(devyreCodexModelsRetryGap)
	if !l.Resolve(context.Background(), "gpt-nobody-serves") {
		t.Fatal("after the gap it asks again")
	}
	if fetched, _ := f.snapshot(); len(fetched) != 2 {
		t.Fatalf("fetched = %v", fetched)
	}
}

func TestDevyreCodexModels_OneLookupAtATime(t *testing.T) {
	f := newFakeCodexModels("codex-a.json")
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
				l.Ensure(context.Background(), "0.163.0")
				results[i] = true
				return
			}
			results[i] = l.Resolve(context.Background(), "gpt-6.1-terra")
		}()
	}
	// Every caller is waiting on the one lookup before it is let through.
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

func TestDevyreCodexModels_RemovedCredentialsAreForgotten(t *testing.T) {
	f := newFakeCodexModels("codex-a.json", "codex-b.json")
	l := f.learner()
	l.Ensure(context.Background(), "0.163.0")
	f.mu.Lock()
	f.credentials = f.credentials[:1]
	f.mu.Unlock()
	l.Ensure(context.Background(), "0.164.0")
	if _, ok := l.store.Get("codex-b.json"); ok {
		t.Fatal("a removed credential's answer is dropped")
	}
}

func TestDevyreWithAccountCodexModels(t *testing.T) {
	store := codexaccountmodels.Default()
	t.Cleanup(func() { store.Retain(nil) })
	catalog := []*ModelInfo{{ID: "gpt-6.1-sol", DisplayName: "catalog's"}}
	if got := devyreWithAccountCodexModels("codex-none.json", catalog); len(got) != 1 {
		t.Fatalf("without an answer the catalog stands: %v", got)
	}
	store.Set("codex-a.json", codexaccountmodels.Learned{
		ClientVersion: "0.163.0",
		Entries: []map[string]any{
			{"slug": "GPT-6.1-SOL", "display_name": "OpenAI's"},
			{"slug": "gpt-7-from-openai", "display_name": "GPT 7"},
		},
	})
	got := devyreWithAccountCodexModels("codex-a.json", catalog)
	if len(got) != 2 || got[0].DisplayName != "catalog's" || got[1].ID != "gpt-7-from-openai" {
		t.Fatalf("models = %+v, want the catalog's kept and the missing one added", got)
	}
}

// Through the real registration: a model only OpenAI lists for the account is
// served by the account, and routing finds it.
func TestRegisterModelsForAuth_CodexAddsTheAccountsOwnModels(t *testing.T) {
	const model = "gpt-devyre-account-only"
	store := codexaccountmodels.Default()
	t.Cleanup(func() { store.Retain(nil) })
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:         "codex-devyre-account.json",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "plus"},
		Metadata:   map[string]any{"access_token": "token"},
	}
	registry := internalregistry.GetGlobalRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)
	if providers := util.GetProviderName(model); len(providers) != 0 {
		t.Fatalf("providers = %v before OpenAI was asked", providers)
	}

	store.Set(auth.ID, codexaccountmodels.Learned{
		ClientVersion: "0.163.0",
		Entries:       []map[string]any{{"slug": model, "display_name": "Account only"}},
	})
	service.registerModelsForAuth(context.Background(), auth)
	providers := util.GetProviderName(model)
	if len(providers) != 1 || providers[0] != "codex" {
		t.Fatalf("providers = %v, want codex", providers)
	}
	found := false
	for _, info := range registry.GetModelsForClient(auth.ID) {
		if info != nil && info.ID == model {
			found = true
		}
	}
	if !found {
		t.Fatal("the account's own model is registered for it")
	}
}
