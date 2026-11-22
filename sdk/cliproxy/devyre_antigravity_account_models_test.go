package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func antigravityFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "antigravityaccountmodels", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func antigravityAnswer(t *testing.T, name string) antigravityaccountmodels.Answer {
	t.Helper()
	answer, err := antigravityaccountmodels.Parse(antigravityFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// fakeAntigravityModels is a learner's world: credentials, a clock, and Google.
type fakeAntigravityModels struct {
	mu          sync.Mutex
	now         time.Time
	credentials []*coreauth.Auth
	bodies      map[string][]byte
	fail        map[string]error
	// gate, when set, holds every lookup until it is closed.
	gate       chan struct{}
	fetched    []string
	registered []string
}

func newFakeAntigravityModels(t *testing.T, ids ...string) *fakeAntigravityModels {
	f := &fakeAntigravityModels{now: time.Unix(1_800_000_000, 0), bodies: map[string][]byte{}, fail: map[string]error{}}
	for _, id := range ids {
		f.credentials = append(f.credentials, &coreauth.Auth{ID: id, Provider: "antigravity", Metadata: map[string]any{"access_token": "ya29." + id}})
		f.bodies[id] = antigravityFixture(t, "fetch_available_models_full.json")
	}
	return f
}

func (f *fakeAntigravityModels) learner() *devyreAntigravityModelLearner {
	l := newDevyreAntigravityModelLearner(nil)
	l.store = antigravityaccountmodels.NewStore()
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
	l.fetch = func(ctx context.Context, auth *coreauth.Auth) ([]byte, error) {
		f.mu.Lock()
		gate := f.gate
		f.fetched = append(f.fetched, auth.ID)
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
		return f.bodies[auth.ID], nil
	}
	l.register = func(auth *coreauth.Auth) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.registered = append(f.registered, auth.ID)
	}
	return l
}

func (f *fakeAntigravityModels) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeAntigravityModels) snapshot() (fetched, registered []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...), append([]string(nil), f.registered...)
}

func (f *fakeAntigravityModels) setToken(id, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, auth := range f.credentials {
		if auth.ID == id {
			clone := auth.Clone()
			clone.Metadata["access_token"] = token
			f.credentials[i] = clone
		}
	}
}

func (f *fakeAntigravityModels) credential(id string) *coreauth.Auth {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, auth := range f.credentials {
		if auth.ID == id {
			return auth
		}
	}
	return nil
}

func idleAntigravity(t *testing.T, l *devyreAntigravityModelLearner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		done, again := l.running, l.again
		l.mu.Unlock()
		if done == nil && !again {
			return
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the lookup did not end")
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the lookups did not end")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDevyreAntigravityModels_EnsureLearnsBeforeTheListIsAnswered(t *testing.T) {
	f := newFakeAntigravityModels(t, "ag-a.json", "ag-b.json")
	l := f.learner()

	l.Ensure(context.Background())
	fetched, registered := f.snapshot()
	if len(fetched) != 2 || len(registered) != 2 {
		t.Fatalf("fetched=%v registered=%v, want both credentials asked and registered before Ensure returned", fetched, registered)
	}
	learned, ok := l.store.Get("ag-a.json")
	if !ok || len(learned.Answer.Served()) != 16 || learned.Credential == "" || strings.Contains(learned.Credential, "ya29") {
		t.Fatalf("learned = %+v", learned)
	}

	// Answered and fresh: nothing is asked again.
	l.Ensure(context.Background())
	if fetched, _ := f.snapshot(); len(fetched) != 2 {
		t.Fatalf("fetched = %v, want no new lookup", fetched)
	}
}

func TestDevyreAntigravityModels_StaleAnswersRefreshBehindTheList(t *testing.T) {
	f := newFakeAntigravityModels(t, "ag-a.json")
	l := f.learner()
	l.Ensure(context.Background())

	f.advance(devyreAntigravityModelsTTL)
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.bodies["ag-a.json"] = antigravityFixture(t, "fetch_available_models_free.json")
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
	idleAntigravity(t, l)
	learned, _ := l.store.Get("ag-a.json")
	if len(learned.Answer.Served()) != 9 {
		t.Fatalf("served = %v, want the refreshed answer", learned.Answer.ServedIDs())
	}
	if _, registered := f.snapshot(); len(registered) != 2 {
		t.Fatalf("registered = %v, want the changed credential registered again", registered)
	}

	// A refreshed token makes the answer stale too: asked behind the list, and
	// the same answer is not registered again.
	f.mu.Lock()
	f.gate = nil
	f.mu.Unlock()
	f.setToken("ag-a.json", "ya29.refreshed")
	f.advance(time.Second)
	l.Ensure(context.Background())
	idleAntigravity(t, l)
	fetched, registered := f.snapshot()
	if len(fetched) != 3 || len(registered) != 2 {
		t.Fatalf("fetched=%v registered=%v", fetched, registered)
	}
	if learned, _ := l.store.Get("ag-a.json"); learned.Credential != devyreAntigravityCredential(f.credential("ag-a.json")) {
		t.Fatal("the answer records the token it was asked with")
	}
}

func TestDevyreAntigravityModels_AFailedLookupKeepsWhatItHad(t *testing.T) {
	f := newFakeAntigravityModels(t, "ag-a.json", "ag-b.json")
	l := f.learner()
	l.Ensure(context.Background())

	f.advance(devyreAntigravityModelsTTL)
	f.mu.Lock()
	f.fail["ag-a.json"] = errors.New("fetchAvailableModels answered 401")
	f.bodies["ag-b.json"] = []byte(`not json`)
	f.mu.Unlock()
	l.Ensure(context.Background())
	idleAntigravity(t, l)
	for _, id := range []string{"ag-a.json", "ag-b.json"} {
		if learned, ok := l.store.Get(id); !ok || len(learned.Answer.Served()) != 16 {
			t.Fatalf("%s learned = %+v, want the earlier answer kept", id, learned)
		}
	}

	// A credential that never answered is not waited for again within the gap.
	g := newFakeAntigravityModels(t, "ag-c.json")
	g.fail["ag-c.json"] = errors.New("down")
	lc := g.learner()
	lc.Ensure(context.Background())
	before, _ := g.snapshot()
	lc.Ensure(context.Background())
	lc.noteRegistered(g.credential("ag-c.json"))
	idleAntigravity(t, lc)
	if after, _ := g.snapshot(); len(after) != len(before) {
		t.Fatalf("fetched %v after %v, want no lookup while the failure is recent", after, before)
	}
	// A new token is asked at once.
	g.mu.Lock()
	delete(g.fail, "ag-c.json")
	g.mu.Unlock()
	g.setToken("ag-c.json", "ya29.new")
	lc.Ensure(context.Background())
	if learned, ok := lc.store.Get("ag-c.json"); !ok || len(learned.Answer.Served()) != 16 {
		t.Fatalf("learned = %+v, want the answer for the new token", learned)
	}
}

func TestDevyreAntigravityModels_ResolveAnUnknownModel(t *testing.T) {
	if newFakeAntigravityModels(t).learner().Resolve(context.Background(), "gemini-4-neon") {
		t.Fatal("without an Antigravity account there is nothing to ask")
	}
	f := newFakeAntigravityModels(t, "ag-a.json")
	l := f.learner()
	if l.Resolve(context.Background(), " ") {
		t.Fatal("no model, nothing to resolve")
	}
	if !l.Resolve(context.Background(), "gemini-4-neon") {
		t.Fatal("the first unknown model starts a lookup and waits for it")
	}
	if l.Resolve(context.Background(), "gemini-nobody-serves") {
		t.Fatal("within the gap an unknown model starts no lookup")
	}
	f.advance(devyreAntigravityModelsRetryGap)
	if !l.Resolve(context.Background(), "gemini-nobody-serves") {
		t.Fatal("after the gap it asks again")
	}
	if fetched, _ := f.snapshot(); len(fetched) != 2 {
		t.Fatalf("fetched = %v", fetched)
	}
}

func TestDevyreAntigravityModels_OneLookupAtATime(t *testing.T) {
	f := newFakeAntigravityModels(t, "ag-a.json")
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
			results[i] = l.Resolve(context.Background(), "gemini-4-neon")
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

// A credential registered (added, or with a new token) without an answer is
// asked at once, without waiting; one registered while a lookup runs is asked
// right after it.
func TestDevyreAntigravityModels_ARegisteredCredentialIsAsked(t *testing.T) {
	f := newFakeAntigravityModels(t, "ag-a.json")
	gate := make(chan struct{})
	f.gate = gate
	l := f.learner()

	l.noteRegistered(f.credential("ag-a.json"))
	deadline := time.Now().Add(5 * time.Second)
	for fetched, _ := f.snapshot(); len(fetched) != 1; fetched, _ = f.snapshot() {
		if time.Now().After(deadline) {
			t.Fatal("the registered credential was not asked")
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	f.credentials = append(f.credentials, &coreauth.Auth{ID: "ag-b.json", Provider: "antigravity", Metadata: map[string]any{"access_token": "ya29.b"}})
	f.bodies["ag-b.json"] = antigravityFixture(t, "fetch_available_models_free.json")
	f.mu.Unlock()
	l.noteRegistered(f.credential("ag-b.json"))
	close(gate)
	idleAntigravity(t, l)
	fetched, registered := f.snapshot()
	if strings.Join(fetched, ",") != "ag-a.json,ag-b.json" || len(registered) != 2 {
		t.Fatalf("fetched=%v registered=%v, want the credential the running lookup missed asked after it", fetched, registered)
	}

	// Registered again with the token it was asked with: nothing to ask.
	l.noteRegistered(f.credential("ag-a.json"))
	idleAntigravity(t, l)
	// With a refreshed token: asked again.
	f.setToken("ag-a.json", "ya29.refreshed")
	l.noteRegistered(f.credential("ag-a.json"))
	idleAntigravity(t, l)
	if fetched, _ := f.snapshot(); strings.Join(fetched, ",") != "ag-a.json,ag-b.json,ag-a.json" {
		t.Fatalf("fetched = %v", fetched)
	}
	// Not an Antigravity credential that can ask: nothing.
	l.noteRegistered(&coreauth.Auth{ID: "ag-c.json", Provider: "antigravity"})
	idleAntigravity(t, l)
	if fetched, _ := f.snapshot(); len(fetched) != 3 {
		t.Fatalf("fetched = %v", fetched)
	}
}

func TestDevyreAntigravityModels_RemovedCredentialsAreForgotten(t *testing.T) {
	f := newFakeAntigravityModels(t, "ag-a.json", "ag-b.json")
	l := f.learner()
	l.Ensure(context.Background())
	l.store.SetRoutes("ag-b.json", []antigravityaccountmodels.Route{{Name: "gemini-4-neon", Default: "gemini-4-neon-xhigh"}})
	f.mu.Lock()
	f.credentials = f.credentials[:1]
	f.mu.Unlock()
	f.advance(devyreAntigravityModelsTTL)
	l.Ensure(context.Background())
	idleAntigravity(t, l)
	if _, ok := l.store.Get("ag-b.json"); ok {
		t.Fatal("a removed credential's answer is dropped")
	}
	if _, ok := l.store.Route("ag-b.json", "gemini-4-neon"); ok {
		t.Fatal("a removed credential's names are dropped")
	}

	// A credential without an access token keeps what it learned, unasked.
	f.setToken("ag-a.json", "")
	f.advance(devyreAntigravityModelsTTL)
	before, _ := f.snapshot()
	l.Ensure(context.Background())
	if l.Resolve(context.Background(), "gemini-4-neon") {
		t.Fatal("no credential can ask")
	}
	idleAntigravity(t, l)
	if after, _ := f.snapshot(); len(after) != len(before) {
		t.Fatalf("fetched %v, want no lookup without a token", after)
	}
	if _, ok := l.store.Get("ag-a.json"); !ok {
		t.Fatal("a credential without a token keeps its answer")
	}
}

func TestDevyreAsksGoogleForModels(t *testing.T) {
	token := map[string]any{"access_token": "ya29.x"}
	for _, tc := range []struct {
		auth *coreauth.Auth
		want bool
	}{
		{&coreauth.Auth{ID: "ag.json", Provider: "antigravity", Metadata: token}, true},
		{&coreauth.Auth{ID: "ag-no-token.json", Provider: "antigravity"}, false},
		{&coreauth.Auth{ID: "ag-off.json", Provider: "antigravity", Metadata: token, Disabled: true}, false},
		{&coreauth.Auth{ID: "ag-status-off.json", Provider: "antigravity", Metadata: token, Status: coreauth.StatusDisabled}, false},
		{&coreauth.Auth{ID: "gemini.json", Provider: "gemini", Metadata: token}, false},
		{nil, false},
	} {
		if got := devyreAsksGoogleForModels(tc.auth); got != tc.want {
			t.Fatalf("%+v: got %v, want %v", tc.auth, got, tc.want)
		}
	}
}

// The learner's request is the capability probe's: its path, token, agent and
// proxy, at every base URL; it names the credential's project.
func TestDevyreFetchAntigravityAvailableModels(t *testing.T) {
	type seen struct{ path, method, auth, agent, contentType, body string }
	var mu sync.Mutex
	var requests []seen
	answer := antigravityFixture(t, "fetch_available_models_full.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{r.URL.Path, r.Method, r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer ya29.refused" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"account secret details"}}`)
			return
		}
		_, _ = w.Write(answer)
	}))
	defer server.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:         "ag-fetch.json",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "ya29.token", "project_id": "my-project-7"},
	}
	body, err := service.devyreFetchAntigravityAvailableModels(context.Background(), auth)
	if err != nil || string(body) != string(answer) {
		t.Fatalf("body=%d bytes err=%v", len(body), err)
	}
	got := requests[0]
	if got.path != "/v1internal:fetchAvailableModels" || got.method != http.MethodPost || got.auth != "Bearer ya29.token" || got.agent != misc.AntigravityUserAgent() || got.contentType != "application/json" {
		t.Fatalf("request = %+v", got)
	}
	if got.body != `{"project":"my-project-7"}` {
		t.Fatalf("body = %s, want the credential's project", got.body)
	}

	// Without a project, the empty request upstream sends.
	delete(auth.Metadata, "project_id")
	if _, err := service.devyreFetchAntigravityAvailableModels(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if requests[1].body != `{}` {
		t.Fatalf("body = %s", requests[1].body)
	}

	// A refusal names its status, never the body.
	auth.Metadata["access_token"] = "ya29.refused"
	_, err = service.devyreFetchAntigravityAvailableModels(context.Background(), auth)
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "ya29") {
		t.Fatalf("err = %v", err)
	}

	// Every base URL at once: the one that answers wins.
	auth.Metadata["access_token"] = "ya29.token"
	auth.Attributes = map[string]string{"base_urls": failing.URL + "," + server.URL}
	if body, err := service.devyreFetchAntigravityAvailableModels(context.Background(), auth); err != nil || len(body) == 0 {
		t.Fatalf("err = %v", err)
	}
	auth.Attributes = map[string]string{"base_url": failing.URL}
	if _, err := service.devyreFetchAntigravityAvailableModels(context.Background(), auth); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}

	delete(auth.Metadata, "access_token")
	if _, err := service.devyreFetchAntigravityAvailableModels(context.Background(), auth); !errors.Is(err, errDevyreAntigravityNoToken) {
		t.Fatalf("err = %v", err)
	}
}

func registeredIDs(clientID string) []string {
	var ids []string
	for _, model := range internalregistry.GetGlobalRegistry().GetModelsForClient(clientID) {
		ids = append(ids, model.ID)
	}
	sort.Strings(ids)
	return ids
}

func registeredInfo(clientID, id string) *ModelInfo {
	for _, model := range internalregistry.GetGlobalRegistry().GetModelsForClient(clientID) {
		if model.ID == id {
			return model
		}
	}
	return nil
}

func antigravityAccount(id string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "antigravity", Status: coreauth.StatusActive}
}

func cleanAntigravityAccounts(t *testing.T, ids ...string) {
	t.Helper()
	registry := internalregistry.GetGlobalRegistry()
	for _, id := range ids {
		registry.UnregisterClient(id)
	}
	t.Cleanup(func() {
		antigravityaccountmodels.Default().Retain(nil)
		for _, id := range ids {
			registry.UnregisterClient(id)
		}
	})
}

// Through the real registration: before Google answers an account serves the
// catalog; after, what Google lists for it and the Gemini API names it serves
// them under, so two accounts with different lists serve different models,
// and the model lists show the names with their display names.
func TestRegisterModelsForAuth_AntigravityServesWhatGoogleListsPerAccount(t *testing.T) {
	full, free := antigravityAccount("ag-devyre-full.json"), antigravityAccount("ag-devyre-free.json")
	cleanAntigravityAccounts(t, full.ID, free.ID)
	store := antigravityaccountmodels.Default()
	service := &Service{cfg: &config.Config{}}

	service.registerModelsForAuth(context.Background(), full)
	var catalog []string
	for _, model := range internalregistry.GetAntigravityModels() {
		catalog = append(catalog, model.ID)
	}
	sort.Strings(catalog)
	if got := registeredIDs(full.ID); strings.Join(got, ",") != strings.Join(catalog, ",") {
		t.Fatalf("before Google answers: %v, want the catalog %v", got, catalog)
	}
	if _, ok := store.Route(full.ID, "gemini-4-neon"); ok {
		t.Fatal("no name is published before Google answers")
	}

	store.Set(full.ID, antigravityaccountmodels.Learned{Answer: antigravityAnswer(t, "fetch_available_models_full.json")})
	store.Set(free.ID, antigravityaccountmodels.Learned{Answer: antigravityAnswer(t, "fetch_available_models_free.json")})
	service.registerModelsForAuth(context.Background(), full)
	service.registerModelsForAuth(context.Background(), free)

	wantFull := "claude-opus-5-5-high,claude-sonnet-5-5,gemini-3.1-pro,gemini-3.1-pro-high,gemini-3.5-flash-lite,gemini-3.7-flash,gemini-3.7-flash-high,gemini-3.8-flash,gemini-3.8-flash-high,gemini-3.8-flash-low,gemini-3.8-flash-medium,gemini-3.8-flash-tiered,gemini-4-carbon,gemini-4-carbon-high,gemini-4-carbon-low,gemini-4-carbon-medium,gemini-4-neon,gemini-4-neon-high,gemini-4-neon-low,gemini-4-neon-medium,gemini-4-neon-xhigh,gpt-oss-120b-medium"
	if got := strings.Join(registeredIDs(full.ID), ","); got != wantFull {
		t.Fatalf("full serves %s,\nwant %s", got, wantFull)
	}
	wantFree := "claude-sonnet-5-5,gemini-3.5-flash-lite,gemini-3.7-flash,gemini-3.7-flash-high,gemini-3.8-flash,gemini-3.8-flash-high,gemini-3.8-flash-low,gemini-3.8-flash-medium,gemini-3.8-flash-tiered,gemini-4-carbon,gemini-4-carbon-high,gemini-4-carbon-low,gemini-4-carbon-medium"
	if got := strings.Join(registeredIDs(free.ID), ","); got != wantFree {
		t.Fatalf("free serves %s,\nwant %s", got, wantFree)
	}
	registry := internalregistry.GetGlobalRegistry()
	if !registry.ClientSupportsModel(full.ID, "gemini-4-neon") || registry.ClientSupportsModel(free.ID, "gemini-4-neon") {
		t.Fatal("only the account that serves Neon is routed gemini-4-neon")
	}
	if providers := util.GetProviderName("gemini-4-neon"); len(providers) != 1 || providers[0] != "antigravity" {
		t.Fatalf("providers = %v", providers)
	}
	if route, ok := store.Route(full.ID, "gemini-4-neon"); !ok || route.Resolve("low") != "gemini-4-neon-low" || route.Resolve("") != "gemini-4-neon-xhigh" {
		t.Fatalf("route = %+v", route)
	}
	if _, ok := store.Route(free.ID, "gemini-4-neon"); ok {
		t.Fatal("the free account publishes no Neon")
	}

	neon := registeredInfo(full.ID, "gemini-4-neon")
	if neon.DisplayName != "Gemini 4 Neon" || strings.Join(neon.Thinking.Levels, ",") != "low,medium,high,xhigh" || neon.ContextLength != 1048576 {
		t.Fatalf("neon = %+v", neon)
	}

	gemini := make(map[string]map[string]any)
	for _, entry := range registry.GetAvailableModels("gemini") {
		name, _ := entry["name"].(string)
		gemini[name] = entry
	}
	for name, display := range map[string]string{
		"gemini-4-neon":    "Gemini 4 Neon",
		"gemini-4-carbon":  "Gemini 4 Carbon",
		"gemini-3.8-flash": "Gemini 3.8 Flash",
		"gemini-3.7-flash": "Gemini 3.8 Flash",
		"gemini-3.1-pro":   "Gemini 3.1 Pro (High)",
	} {
		if entry, ok := gemini[name]; !ok || entry["displayName"] != display {
			t.Fatalf("GET /v1beta/models %s = %v, want display name %q", name, entry, display)
		}
	}
	openai := make(map[string]map[string]any)
	for _, entry := range registry.GetAvailableModels("openai") {
		id, _ := entry["id"].(string)
		openai[id] = entry
	}
	if entry, ok := openai["gemini-4-neon"]; !ok || entry["display_name"] != "Gemini 4 Neon" {
		t.Fatalf("GET /v1/models gemini-4-neon = %v", entry)
	}
}

// Web search follows Google's answer for the account, the names included.
func TestRegisterModelsForAuth_AntigravityWebSearchFromGooglesAnswer(t *testing.T) {
	account := antigravityAccount("ag-devyre-search.json")
	cleanAntigravityAccounts(t, account.ID)
	antigravityaccountmodels.Default().Set(account.ID, antigravityaccountmodels.Learned{Answer: antigravityAnswer(t, "fetch_available_models_full.json")})
	(&Service{cfg: &config.Config{}}).registerModelsForAuth(context.Background(), account)
	for id, want := range map[string]bool{
		"gemini-3.5-flash-lite": true, // the catalog has no flag; Google lists it
		"gemini-4-neon-xhigh":   true,
		"gemini-4-neon":         true, // its default variant
		"gemini-4-carbon-low":   false,
		"gemini-4-carbon":       true,
	} {
		info := registeredInfo(account.ID, id)
		if info == nil || info.SupportsWebSearch != want {
			t.Fatalf("%s = %+v, want web search %v", id, info, want)
		}
	}
}

// A configured oauth-model-alias keeps its name; the account's other names
// stand. Excluded ids are not served, nor reached through a name.
func TestRegisterModelsForAuth_AntigravityLeavesConfiguredNamesAndExclusionsAlone(t *testing.T) {
	account := antigravityAccount("ag-devyre-configured.json")
	account.Attributes = map[string]string{"excluded_models": "gemini-4-neon-xhigh,gemini-3.1-pro"}
	cleanAntigravityAccounts(t, account.ID)
	store := antigravityaccountmodels.Default()
	store.Set(account.ID, antigravityaccountmodels.Learned{Answer: antigravityAnswer(t, "fetch_available_models_full.json")})
	service := &Service{cfg: &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"antigravity": {{Name: "gemini-3.8-flash-medium", Alias: "gemini-3.8-flash", Fork: true}},
	}}}
	service.registerModelsForAuth(context.Background(), account)

	if _, ok := store.Route(account.ID, "gemini-3.8-flash"); ok {
		t.Fatal("a configured alias's name is left to it")
	}
	if flash := registeredInfo(account.ID, "gemini-3.8-flash"); flash == nil || flash.DisplayName != "Gemini 3.8 Flash (Medium)" {
		t.Fatalf("gemini-3.8-flash = %+v, want the configured alias", flash)
	}
	if _, ok := store.Route(account.ID, "gemini-4-carbon"); !ok {
		t.Fatal("the other names stand")
	}
	for _, id := range []string{"gemini-4-neon-xhigh", "gemini-3.1-pro", "gemini-3.1-pro-high"} {
		if id == "gemini-3.1-pro-high" {
			if registeredInfo(account.ID, id) == nil {
				t.Fatal("an id the exclusion does not name stays")
			}
			continue
		}
		if registeredInfo(account.ID, id) != nil {
			t.Fatalf("%s is excluded", id)
		}
	}
	if _, ok := store.Route(account.ID, "gemini-3.1-pro"); ok {
		t.Fatal("an excluded name is not published")
	}
	if route, ok := store.Route(account.ID, "gemini-4-neon"); !ok || route.Resolve("") != "gemini-4-neon-low" || route.Resolve("xhigh") != "gemini-4-neon-low" {
		t.Fatalf("route = %+v, want the excluded default never reached", route)
	}
}

// The whole path against a stand-in for Google's Code Assist API: two accounts
// are added, the learner asks Google for each (in the place of the capability
// probe, with the account's project), and a request for a Gemini API name
// reaches Google as the variant for its level, on the account that serves it.
func TestAntigravityAccountModels_EndToEnd(t *testing.T) {
	type call struct{ token, path, body string }
	var mu sync.Mutex
	var calls []call
	full := antigravityFixture(t, "fetch_available_models_full.json")
	free := antigravityFixture(t, "fetch_available_models_free.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls = append(calls, call{token, r.URL.Path, string(body)})
		mu.Unlock()
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			if token == "ya29.free" {
				_, _ = w.Write(free)
				return
			}
			_, _ = w.Write(full)
		case "/v1internal:generateContent":
			_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	account := func(id, token string) *coreauth.Auth {
		return &coreauth.Auth{
			ID:         id,
			Provider:   "antigravity",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"base_url": server.URL},
			Metadata: map[string]any{
				"access_token": token,
				"project_id":   "project-" + strings.TrimPrefix(token, "ya29."),
				"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
	}
	fullAuth, freeAuth := account("ag-devyre-e2e-full.json", "ya29.full"), account("ag-devyre-e2e-free.json", "ya29.free")
	cleanAntigravityAccounts(t, fullAuth.ID, freeAuth.ID)
	t.Cleanup(func() {
		devyreAntigravityLearner.Store(nil)
		antigravityaccountmodels.SetLearner(nil)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{RequestRetry: 1}, coreManager: manager}
	service.devyreStartAntigravityAccountModels()
	service.applyCoreAuthAddOrUpdate(context.Background(), fullAuth)
	service.applyCoreAuthAddOrUpdate(context.Background(), freeAuth)

	deadline := time.Now().Add(10 * time.Second)
	store := antigravityaccountmodels.Default()
	for {
		_, fullReady := store.Route(fullAuth.ID, "gemini-4-neon")
		_, freeReady := store.Route(freeAuth.ID, "gemini-4-carbon")
		if fullReady && freeReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the accounts were not learned: calls %+v", calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	service.WaitAntigravityProbes()

	mu.Lock()
	for _, c := range calls {
		if c.path != "/v1internal:fetchAvailableModels" {
			continue
		}
		if c.body != `{"project":"project-`+strings.TrimPrefix(c.token, "ya29.")+`"}` {
			t.Fatalf("fetchAvailableModels body = %s: the capability probe asked beside the learner", c.body)
		}
	}
	asked := len(calls)
	mu.Unlock()
	if asked != 2 {
		t.Fatalf("asked Google %d times, want once per account", asked)
	}

	execute := func(model, level string) call {
		t.Helper()
		payload := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		if level != "" {
			payload = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}}}`
		}
		_, err := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model, Payload: []byte(payload)}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatGemini,
			ResponseFormat:  sdktranslator.FormatGemini,
			OriginalRequest: []byte(payload),
		})
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return calls[len(calls)-1]
	}
	for _, tc := range []struct{ model, level, want, token string }{
		{"gemini-4-neon", "LOW", "gemini-4-neon-low", "ya29.full"},
		{"gemini-4-neon", "", "gemini-4-neon-xhigh", "ya29.full"},
		{"gemini-4-neon", "HIGH", "gemini-4-neon-high", "ya29.full"},
		{"gemini-3.7-flash", "LOW", "gemini-3.8-flash-low", ""},
		{"gemini-4-carbon", "", "gemini-4-carbon-high", ""},
		{"gemini-3.8-flash-tiered", "", "gemini-3.8-flash-tiered", ""},
	} {
		got := execute(tc.model, tc.level)
		if got.path != "/v1internal:generateContent" {
			t.Fatalf("%s: last call %+v", tc.model, got)
		}
		if model := gjson.Get(got.body, "model").String(); model != tc.want {
			t.Fatalf("%s at %q reached Google as %q, want %q", tc.model, tc.level, model, tc.want)
		}
		if tc.token != "" && got.token != tc.token {
			t.Fatalf("%s went to the account of %s", tc.model, got.token)
		}
	}
}
