package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// devyre: the models Google offers each Antigravity account, asked of Google
// with the account's own credential (internal/antigravityaccountmodels has the
// why). A lookup asks the Antigravity credentials that need it, several at
// once, one lookup at a time:
//
//   - when a Gemini client asks for its model list (GET /v1beta/models); the
//     list waits for credentials that have no answer yet, and stale answers
//     are refreshed behind it;
//   - when a credential is registered (added, or changed: a refreshed token, a
//     new sign-in) without an answer for its current token. This takes the
//     place of upstream's capability probe, whose request it is: one answer
//     serves both the models and their web-search hints;
//   - when a request names a model no credential serves: it waits for a running
//     lookup, or starts one unless one ended within devyreAntigravityModelsRetryGap;
//   - once at start.
//
// A credential is asked at most once per devyreAntigravityModelsRetryGap for
// the same token. One whose lookup fails keeps what it had. What changed is
// registered again at once, so routing and the model lists follow without a
// restart.
const (
	// devyreAntigravityModelsFetchTimeout bounds one credential's lookup.
	devyreAntigravityModelsFetchTimeout = 15 * time.Second
	// devyreAntigravityModelsWait is the longest a model list or a request waits.
	devyreAntigravityModelsWait = 10 * time.Second
	// devyreAntigravityModelsTTL is how long an answer stands before it is refreshed.
	devyreAntigravityModelsTTL = 15 * time.Minute
	// devyreAntigravityModelsRetryGap spaces the lookups an unknown model
	// starts, and the times one credential is asked.
	devyreAntigravityModelsRetryGap = time.Minute
	// devyreAntigravityModelsWorkers bounds the credentials asked at once.
	devyreAntigravityModelsWorkers = 4
)

// devyreAntigravityLearner is the learner Run installed, which takes the place
// of the capability probe for its service.
var devyreAntigravityLearner atomic.Pointer[devyreAntigravityModelLearner]

type devyreAntigravityFailure struct {
	at         time.Time
	credential string
}

type devyreAntigravityModelLearner struct {
	s     *Service
	store *antigravityaccountmodels.Store
	now   func() time.Time
	// fetch asks Google for one credential's models (the service's request,
	// the capability probe's; tests replace it).
	fetch func(ctx context.Context, auth *coreauth.Auth) ([]byte, error)
	// credentials lists the enabled Antigravity credentials (tests replace it);
	// the ones with an access token are asked.
	credentials func() []*coreauth.Auth
	// register registers a credential's models again (tests replace it).
	register func(auth *coreauth.Auth)

	mu sync.Mutex
	// running is closed when the running lookup ends; nil when none runs.
	running chan struct{}
	// again asks for another lookup once the running one ends, for a
	// credential registered while it ran.
	again bool
	// lastEnded is when the last lookup ended.
	lastEnded time.Time
	// failed is each credential's last failed lookup.
	failed map[string]devyreAntigravityFailure
}

func newDevyreAntigravityModelLearner(s *Service) *devyreAntigravityModelLearner {
	l := &devyreAntigravityModelLearner{
		s:      s,
		store:  antigravityaccountmodels.Default(),
		now:    time.Now,
		failed: make(map[string]devyreAntigravityFailure),
	}
	l.fetch = func(ctx context.Context, auth *coreauth.Auth) ([]byte, error) {
		return l.s.devyreFetchAntigravityAvailableModels(ctx, auth)
	}
	l.credentials = l.serviceCredentials
	l.register = func(auth *coreauth.Auth) {
		if l.s != nil {
			l.s.refreshModelRegistrationForAuth(auth)
		}
	}
	return l
}

// devyreStartAntigravityAccountModels installs the learner for the request
// paths and the registration, and asks Google once for the credentials loaded
// at start.
func (s *Service) devyreStartAntigravityAccountModels() {
	if s == nil || s.coreManager == nil {
		return
	}
	learner := newDevyreAntigravityModelLearner(s)
	devyreAntigravityLearner.Store(learner)
	antigravityaccountmodels.SetLearner(learner)
	go learner.Ensure(context.Background())
}

// devyreAntigravityLearnerProbes reports whether this service's learner takes
// the place of the capability probe for a credential just registered, and
// starts its lookup when the credential has no answer for its current token.
func (s *Service) devyreAntigravityLearnerProbes(auth *coreauth.Auth) bool {
	learner := devyreAntigravityLearner.Load()
	if s == nil || learner == nil || learner.s != s {
		return false
	}
	learner.noteRegistered(auth)
	return true
}

// devyreWithAccountAntigravityModels makes an Antigravity credential's models
// the ones Google lists for its account, once Google has answered: the models
// that are neither disabled nor internal, each described by the catalog when
// it knows the id and by Google's entry when it does not, marked for Google
// Search as Google's answer lists them. To these it adds the Gemini API names
// the account serves them under (antigravityaccountmodels.Routes), which it
// publishes for the executor; the ids stay listed too.
//
// models are the catalog's, less what excluded names; the same exclusions
// apply to Google's models and to the names. A name an oauth-model-alias entry
// for the credential gives (in the config or on the credential) is left to
// that entry. Before Google has answered, or when its answer lists nothing,
// the catalog stands and no name is published.
func (s *Service) devyreWithAccountAntigravityModels(a *coreauth.Auth, models []*ModelInfo, excluded []string) []*ModelInfo {
	if a == nil || a.ID == "" {
		return models
	}
	store := antigravityaccountmodels.Default()
	learned, ok := store.Get(a.ID)
	if !ok || len(learned.Answer.Served()) == 0 {
		store.SetRoutes(a.ID, nil)
		return models
	}
	served := applyExcludedModels(antigravityaccountmodels.ModelInfos(learned.Answer), excluded)
	byID := make(map[string]*ModelInfo, len(served))
	ids := make([]string, 0, len(served))
	for _, info := range served {
		byID[strings.ToLower(info.ID)] = info
		ids = append(ids, info.ID)
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	claimed := make(map[string]struct{})
	for _, alias := range oauthModelAliasesForAuth(cfg, coreauth.OAuthModelAliasChannel(a.Provider, a.AuthKind()), a.Attributes) {
		if name := strings.ToLower(strings.TrimSpace(alias.Alias)); name != "" {
			claimed[name] = struct{}{}
		}
	}
	var routes []antigravityaccountmodels.Route
	var named []*ModelInfo
	for _, route := range antigravityaccountmodels.Routes(learned.Answer, ids) {
		if _, taken := claimed[strings.ToLower(route.Name)]; taken {
			continue
		}
		info := antigravityaccountmodels.RouteInfo(route, byID)
		if info == nil || len(applyExcludedModels([]*ModelInfo{info}, excluded)) == 0 {
			continue
		}
		routes = append(routes, route)
		named = append(named, info)
	}
	store.SetRoutes(a.ID, routes)
	return append(served, named...)
}

func (l *devyreAntigravityModelLearner) Ensure(ctx context.Context) {
	credentials := l.credentials()
	if len(credentials) == 0 {
		return
	}
	now := l.now()
	missing, stale := false, false
	for _, auth := range credentials {
		if !devyreAsksGoogleForModels(auth) || l.recentlyFailed(auth, now) {
			continue
		}
		learned, ok := l.store.Get(auth.ID)
		switch {
		case !ok:
			missing = true
		case learned.Credential != devyreAntigravityCredential(auth) || now.Sub(learned.At) >= devyreAntigravityModelsTTL:
			stale = true
		}
	}
	if !missing && !stale {
		return
	}
	done := l.start()
	if missing {
		l.wait(ctx, done)
	}
}

func (l *devyreAntigravityModelLearner) Resolve(ctx context.Context, model string) bool {
	if strings.TrimSpace(model) == "" || !l.anyAsks() {
		return false
	}
	l.mu.Lock()
	done := l.running
	recent := !l.lastEnded.IsZero() && l.now().Sub(l.lastEnded) < devyreAntigravityModelsRetryGap
	l.mu.Unlock()
	if done == nil {
		if recent {
			return false
		}
		done = l.start()
	}
	return l.wait(ctx, done)
}

// noteRegistered starts a lookup for a credential just registered that has no
// answer for its current token, without waiting for it.
func (l *devyreAntigravityModelLearner) noteRegistered(auth *coreauth.Auth) {
	if !devyreAsksGoogleForModels(auth) {
		return
	}
	if learned, ok := l.store.Get(auth.ID); ok && learned.Credential == devyreAntigravityCredential(auth) {
		return
	}
	if l.recentlyFailed(auth, l.now()) {
		return
	}
	l.mu.Lock()
	if l.running != nil {
		// The running lookup may have listed the credentials before this one.
		l.again = true
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	l.start()
}

// recentlyFailed reports whether the credential's lookup failed within the
// gap with its current token.
func (l *devyreAntigravityModelLearner) recentlyFailed(auth *coreauth.Auth, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	failure, ok := l.failed[auth.ID]
	return ok && failure.credential == devyreAntigravityCredential(auth) && now.Sub(failure.at) < devyreAntigravityModelsRetryGap
}

// anyAsks reports whether a credential can ask Google.
func (l *devyreAntigravityModelLearner) anyAsks() bool {
	for _, auth := range l.credentials() {
		if devyreAsksGoogleForModels(auth) {
			return true
		}
	}
	return false
}

// due reports whether a lookup asks the credential: it can, has no answer for
// its current token or one older than the gap, and did not just fail.
func (l *devyreAntigravityModelLearner) due(auth *coreauth.Auth, now time.Time) bool {
	if !devyreAsksGoogleForModels(auth) || l.recentlyFailed(auth, now) {
		return false
	}
	learned, ok := l.store.Get(auth.ID)
	return !ok || learned.Credential != devyreAntigravityCredential(auth) || now.Sub(learned.At) >= devyreAntigravityModelsRetryGap
}

// start begins a lookup unless one already runs, and returns the channel closed
// when the lookup ends.
func (l *devyreAntigravityModelLearner) start() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running != nil {
		return l.running
	}
	done := make(chan struct{})
	l.running = done
	go func() {
		l.run()
		l.mu.Lock()
		l.running = nil
		l.lastEnded = l.now()
		again := l.again
		l.again = false
		l.mu.Unlock()
		close(done)
		if again {
			l.start()
		}
	}()
	return done
}

// wait waits for done, at most devyreAntigravityModelsWait and while ctx
// lives, and reports whether the lookup ended.
func (l *devyreAntigravityModelLearner) wait(ctx context.Context, done <-chan struct{}) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(devyreAntigravityModelsWait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (l *devyreAntigravityModelLearner) run() {
	credentials := l.credentials()
	keep := make(map[string]struct{}, len(credentials))
	for _, auth := range credentials {
		keep[auth.ID] = struct{}{}
	}
	l.store.Retain(keep)
	now := l.now()
	var due []*coreauth.Auth
	for _, auth := range credentials {
		if l.due(auth, now) {
			due = append(due, auth)
		}
	}
	if len(due) == 0 {
		return
	}
	work := make(chan *coreauth.Auth)
	var wg sync.WaitGroup
	workers := min(devyreAntigravityModelsWorkers, len(due))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for auth := range work {
				l.learn(auth)
			}
		}()
	}
	for _, auth := range due {
		work <- auth
	}
	close(work)
	wg.Wait()
}

func (l *devyreAntigravityModelLearner) learn(auth *coreauth.Auth) {
	ctx, cancel := context.WithTimeout(context.Background(), devyreAntigravityModelsFetchTimeout)
	defer cancel()
	credential := devyreAntigravityCredential(auth)
	body, errFetch := l.fetch(ctx, auth)
	var answer antigravityaccountmodels.Answer
	if errFetch == nil {
		answer, errFetch = antigravityaccountmodels.Parse(body)
	}
	if errFetch != nil {
		l.mu.Lock()
		l.failed[auth.ID] = devyreAntigravityFailure{at: l.now(), credential: credential}
		l.mu.Unlock()
		log.WithError(errFetch).Warnf("antigravity account models: could not ask Google for auth=%s; keeping what it had", auth.ID)
		return
	}
	l.mu.Lock()
	delete(l.failed, auth.ID)
	l.mu.Unlock()
	learned := antigravityaccountmodels.Learned{At: l.now(), Credential: credential, Answer: answer}
	if !l.store.Set(auth.ID, learned) {
		log.Debugf("antigravity account models: auth=%s unchanged", auth.ID)
		return
	}
	if ids := answer.ServedIDs(); len(ids) > 0 {
		log.Infof("antigravity account models: auth=%s models=%d (%s)", auth.ID, len(ids), strings.Join(ids, ","))
	} else {
		log.Infof("antigravity account models: auth=%s Google lists no models it serves; the catalog stands", auth.ID)
	}
	l.register(auth)
}

// serviceCredentials lists the enabled Antigravity credentials. One without an
// access token keeps what it learned but is not asked until it has one.
func (l *devyreAntigravityModelLearner) serviceCredentials() []*coreauth.Auth {
	if l.s == nil || l.s.coreManager == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range l.s.coreManager.List() {
		if devyreAntigravityCredentialEnabled(auth) {
			out = append(out, auth)
		}
	}
	return out
}

func devyreAntigravityCredentialEnabled(auth *coreauth.Auth) bool {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity")
}

// devyreAsksGoogleForModels reports whether the learner asks Google for the
// credential: an enabled Antigravity credential with an access token.
func devyreAsksGoogleForModels(auth *coreauth.Auth) bool {
	return devyreAntigravityCredentialEnabled(auth) && devyreAntigravityAccessToken(auth) != ""
}

// devyreAntigravityAccessToken is the token the capability probe asks with.
func devyreAntigravityAccessToken(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token)
}

// devyreAntigravityCredential identifies the access token an answer was asked
// with, without keeping the token.
func devyreAntigravityCredential(auth *coreauth.Auth) string {
	token := devyreAntigravityAccessToken(auth)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// devyreAntigravityModelsRequest is the fetchAvailableModels request
// (FetchAvailableModelsRequest): the credential's project, which identifies
// the account's Code Assist project, when it has one; else empty, as upstream
// asks.
func devyreAntigravityModelsRequest(auth *coreauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		if project, _ := auth.Metadata["project_id"].(string); strings.TrimSpace(project) != "" {
			if body, errMarshal := json.Marshal(map[string]string{"project": strings.TrimSpace(project)}); errMarshal == nil {
				return string(body)
			}
		}
	}
	return `{}`
}

// devyreFetchAntigravityAvailableModels asks Google what the credential's
// account may use: the capability probe's request (its hosts, headers, token
// and proxy), at every base URL the credential names at once, the first answer
// winning. It returns the body of a 2xx answer; an error names the status, not
// the body.
func (s *Service) devyreFetchAntigravityAvailableModels(ctx context.Context, auth *coreauth.Auth) ([]byte, error) {
	token := devyreAntigravityAccessToken(auth)
	if token == "" {
		return nil, errDevyreAntigravityNoToken
	}
	baseURLs := antigravityModelBaseURLs(auth)
	client := &http.Client{Timeout: devyreAntigravityModelsFetchTimeout}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth)); errProxy == nil && transport != nil {
		client.Transport = transport
	}
	payload := devyreAntigravityModelsRequest(auth)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		body []byte
		err  error
	}
	results := make(chan result, len(baseURLs))
	for _, baseURL := range baseURLs {
		go func(baseURL string) {
			body, _, errFetch := fetchAntigravityAvailableModels(ctx, client, baseURL, token, payload)
			results <- result{body: body, err: errFetch}
		}(baseURL)
	}
	var lastErr error
	for range baseURLs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case answer := <-results:
			if answer.err == nil {
				return answer.body, nil
			}
			lastErr = answer.err
		}
	}
	return nil, fmt.Errorf("antigravity account models: %w", lastErr)
}

type devyreAntigravityModelsError string

func (e devyreAntigravityModelsError) Error() string { return string(e) }

const errDevyreAntigravityNoToken = devyreAntigravityModelsError("antigravity account models: the credential has no access token")
