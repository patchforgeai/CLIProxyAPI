package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
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
//     list waits for credentials that have no answer yet, and answers older
//     than devyreAntigravityModelsTTL are refreshed behind it;
//   - when a credential is registered (added, or changed: a refreshed token, a
//     new sign-in) without an answer for its current token;
//   - when a request names a model no credential serves: it waits for a running
//     lookup, or starts one unless one ended within devyreAntigravityModelsRetryGap;
//   - on its own, for answers older than devyreAntigravityModelsRefresh, since
//     an answer can change under the same token (a model Google disables for a
//     while);
//   - once at start.
//
// Its request is upstream's capability probe's, and its answer carries the
// probe's web-search hints: for an account it has an answer for, the probe
// asks nothing of its own (devyreAntigravityLearnerProbes).
//
// A credential whose lookup fails keeps what it had. What changed is
// registered again at once, so routing and the model lists follow without a
// restart. Shutdown stops the learner: a lookup running then registers nothing.
const (
	// devyreAntigravityModelsFetchTimeout bounds one credential's lookup.
	devyreAntigravityModelsFetchTimeout = 15 * time.Second
	// devyreAntigravityModelsWait is the longest a model list or a request waits.
	devyreAntigravityModelsWait = 10 * time.Second
	// devyreAntigravityModelsTTL is how old an answer gets before a model list
	// refreshes it.
	devyreAntigravityModelsTTL = 15 * time.Minute
	// devyreAntigravityModelsRefresh is how old an answer gets before the
	// learner asks again on its own.
	devyreAntigravityModelsRefresh = 30 * time.Minute
	// devyreAntigravityModelsTick is how often the learner looks for such answers.
	devyreAntigravityModelsTick = 5 * time.Minute
	// devyreAntigravityModelsRetryGap spaces the lookups an unknown model
	// starts, and a failed credential's next attempt with the same token.
	devyreAntigravityModelsRetryGap = time.Minute
	// devyreAntigravityModelsNewOnly asks only the credentials without an
	// answer for their current token.
	devyreAntigravityModelsNewOnly = time.Duration(math.MaxInt64)
	// devyreAntigravityModelsWorkers bounds the credentials asked at once.
	devyreAntigravityModelsWorkers = 4
)

// devyreAntigravityLearner is the learner Run installed.
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
	// runningAge is the age from which the running lookup asks an answer again.
	runningAge time.Duration
	// follow asks for another lookup, from followAge, once the running one
	// ends: for a credential registered while it ran, or answers it leaves.
	follow    bool
	followAge time.Duration
	// lastEnded is when the last lookup that asked a credential ended.
	lastEnded time.Time
	// failed is each credential's last failed lookup.
	failed map[string]devyreAntigravityFailure
	// stopped is set, and stop closed, when the service shuts down.
	stopped bool
	stop    chan struct{}
}

func newDevyreAntigravityModelLearner(s *Service) *devyreAntigravityModelLearner {
	l := &devyreAntigravityModelLearner{
		s:      s,
		store:  antigravityaccountmodels.Default(),
		now:    time.Now,
		failed: make(map[string]devyreAntigravityFailure),
		stop:   make(chan struct{}),
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
// paths and the registration, asks Google once for the credentials loaded at
// start, and refreshes old answers until Shutdown.
func (s *Service) devyreStartAntigravityAccountModels() {
	if s == nil || s.coreManager == nil {
		return
	}
	learner := newDevyreAntigravityModelLearner(s)
	devyreAntigravityLearner.Store(learner)
	antigravityaccountmodels.SetLearner(learner)
	go learner.Ensure(context.Background())
	go learner.refreshEvery(devyreAntigravityModelsTick)
}

// devyreStopAntigravityAccountModels stops this service's learner: its own
// refreshes end, a lookup running now registers nothing, and the request paths
// no longer ask it.
func (s *Service) devyreStopAntigravityAccountModels() {
	learner := devyreAntigravityLearner.Load()
	if s == nil || learner == nil || learner.s != s {
		return
	}
	learner.halt()
	if devyreAntigravityLearner.CompareAndSwap(learner, nil) {
		antigravityaccountmodels.SetLearner(nil)
	}
}

// devyreAntigravityLearnerProbes starts the lookup of a credential just
// registered that has no answer for its current token, and reports whether
// this service's learner takes the place of the capability probe for it: when
// it has an answer for the account that serves models, whose web-search hints
// the registration applied. Without one (not asked yet, failed, or serving
// nothing) the probe runs as before.
func (s *Service) devyreAntigravityLearnerProbes(auth *coreauth.Auth) bool {
	learner := devyreAntigravityLearner.Load()
	if s == nil || auth == nil || learner == nil || learner.s != s {
		return false
	}
	learner.noteRegistered(auth)
	learned, ok := learner.store.Get(auth.ID)
	return ok && len(learned.Answer.Served()) > 0
}

// devyreWithAccountAntigravityModels makes an Antigravity credential's models
// the ones Google lists for its account, once Google has answered
// (antigravityaccountmodels.Answer.Served), each described by the catalog
// when it knows the id and by Google's entry when it does not, marked for
// Google Search as Google's answer lists them. To these it adds the Gemini API
// names the account serves them under (antigravityaccountmodels.Routes), which
// it publishes for the executor; the ids stay listed too.
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
	done := l.start(devyreAntigravityModelsTTL, false)
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
		done = l.start(devyreAntigravityModelsRetryGap, false)
	}
	return l.wait(ctx, done)
}

// noteRegistered starts a lookup for a credential just registered that has no
// answer for its current token, without waiting for it; it asks no other
// credential that has one.
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
	// A running lookup may have listed the credentials before this one.
	l.start(devyreAntigravityModelsNewOnly, true)
}

// refreshEvery asks again, every tick, for the answers older than
// devyreAntigravityModelsRefresh, until the learner stops.
func (l *devyreAntigravityModelLearner) refreshEvery(tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.refreshOld()
		}
	}
}

// refreshOld starts a lookup of the answers older than
// devyreAntigravityModelsRefresh, and reports whether there were any.
func (l *devyreAntigravityModelLearner) refreshOld() bool {
	now := l.now()
	for _, auth := range l.credentials() {
		if l.due(auth, now, devyreAntigravityModelsRefresh) {
			l.start(devyreAntigravityModelsRefresh, false)
			return true
		}
	}
	return false
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

// recentlyFailed reports whether the credential's lookup failed within the
// gap with its current token.
func (l *devyreAntigravityModelLearner) recentlyFailed(auth *coreauth.Auth, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	failure, ok := l.failed[auth.ID]
	return ok && failure.credential == devyreAntigravityCredential(auth) && now.Sub(failure.at) < devyreAntigravityModelsRetryGap
}

// due reports whether a lookup from minAge asks the credential: it can, did
// not just fail, and has no answer for its current token or one at least
// minAge old.
func (l *devyreAntigravityModelLearner) due(auth *coreauth.Auth, now time.Time, minAge time.Duration) bool {
	if !devyreAsksGoogleForModels(auth) || l.recentlyFailed(auth, now) {
		return false
	}
	learned, ok := l.store.Get(auth.ID)
	return !ok || learned.Credential != devyreAntigravityCredential(auth) || now.Sub(learned.At) >= minAge
}

func (l *devyreAntigravityModelLearner) isStopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

func (l *devyreAntigravityModelLearner) halt() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stopped {
		l.stopped = true
		close(l.stop)
	}
}

// start begins a lookup that asks the credentials due from minAge unless one
// already runs, and returns the channel closed when the lookup ends. When one
// runs that asks less (from an older age), or after is set, another lookup
// follows it. A stopped learner starts none.
func (l *devyreAntigravityModelLearner) start(minAge time.Duration, after bool) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		done := make(chan struct{})
		close(done)
		return done
	}
	if l.running != nil {
		if after || minAge < l.runningAge {
			if !l.follow || minAge < l.followAge {
				l.followAge = minAge
			}
			l.follow = true
		}
		return l.running
	}
	done := make(chan struct{})
	l.running, l.runningAge = done, minAge
	go func() {
		asked := l.run(minAge)
		l.mu.Lock()
		l.running = nil
		if asked {
			l.lastEnded = l.now()
		}
		follow, age := l.follow, l.followAge
		l.follow = false
		l.mu.Unlock()
		close(done)
		if follow {
			l.start(age, false)
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

// run asks the credentials due from minAge, forgets removed ones, and reports
// whether it asked any.
func (l *devyreAntigravityModelLearner) run(minAge time.Duration) bool {
	credentials := l.credentials()
	keep := make(map[string]struct{}, len(credentials))
	for _, auth := range credentials {
		keep[auth.ID] = struct{}{}
	}
	l.store.Retain(keep)
	l.mu.Lock()
	for id := range l.failed {
		if _, ok := keep[id]; !ok {
			delete(l.failed, id)
		}
	}
	l.mu.Unlock()
	now := l.now()
	var due []*coreauth.Auth
	for _, auth := range credentials {
		if l.due(auth, now, minAge) {
			due = append(due, auth)
		}
	}
	if len(due) == 0 {
		return false
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
	return true
}

func (l *devyreAntigravityModelLearner) learn(auth *coreauth.Auth) {
	ctx, cancel := context.WithTimeout(context.Background(), devyreAntigravityModelsFetchTimeout)
	defer cancel()
	credential := devyreAntigravityCredential(auth)
	body, errFetch := l.fetch(ctx, auth)
	if l.isStopped() {
		return
	}
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
	if l.isStopped() {
		return
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
