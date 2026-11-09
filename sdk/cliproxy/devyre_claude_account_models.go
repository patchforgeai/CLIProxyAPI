package cliproxy

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudeaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// devyre: the models Anthropic offers each Claude account, asked of Anthropic
// with the account's own credential (internal/claudeaccountmodels has the why).
// A lookup asks every Claude credential that reaches Anthropic at once, one
// lookup at a time:
//
//   - when a Claude client asks for its model list (GET /v1/models); the list
//     waits for credentials that have no answer yet, and stale answers are
//     refreshed behind it;
//   - when a request names a model no credential serves: it waits for a running
//     lookup, or starts one unless one ended within devyreClaudeModelsRetryGap;
//   - once at start.
//
// A credential whose lookup fails keeps what it had. What changed is registered
// again at once, so routing and the model lists follow without a restart.
const (
	// devyreClaudeModelsFetchTimeout bounds one credential's lookup.
	devyreClaudeModelsFetchTimeout = 15 * time.Second
	// devyreClaudeModelsWait is the longest a model list or a request waits.
	devyreClaudeModelsWait = 10 * time.Second
	// devyreClaudeModelsTTL is how long an answer stands before it is refreshed.
	devyreClaudeModelsTTL = 15 * time.Minute
	// devyreClaudeModelsRetryGap spaces the lookups an unknown model starts,
	// and a failed credential's next attempt.
	devyreClaudeModelsRetryGap = time.Minute
	// devyreClaudeModelsWorkers bounds the credentials asked at once.
	devyreClaudeModelsWorkers = 4
	// devyreClaudeModelsMaxPages bounds the pages read for one credential.
	devyreClaudeModelsMaxPages = 10
)

// devyreClaudeModelsFetcher is the Claude executor's lookup
// (executor.ClaudeExecutor.DevyreFetchAccountModels).
type devyreClaudeModelsFetcher interface {
	DevyreFetchAccountModels(ctx context.Context, auth *coreauth.Auth, afterID string) ([]byte, error)
}

type devyreClaudeModelLearner struct {
	s     *Service
	store *claudeaccountmodels.Store
	now   func() time.Time
	// fetch asks Anthropic for one page of one credential's list (the
	// executor's; tests replace it).
	fetch func(ctx context.Context, auth *coreauth.Auth, afterID string) ([]byte, error)
	// credentials lists the Claude credentials to ask (tests replace it).
	credentials func() []*coreauth.Auth
	// register registers a credential's models again (tests replace it).
	register func(auth *coreauth.Auth)

	mu sync.Mutex
	// running is closed when the running lookup ends; nil when none runs.
	running chan struct{}
	// lastEnded is when the last lookup that asked a credential ended.
	lastEnded time.Time
	// failedAt is when each credential's last lookup failed.
	failedAt map[string]time.Time
}

func newDevyreClaudeModelLearner(s *Service) *devyreClaudeModelLearner {
	l := &devyreClaudeModelLearner{
		s:        s,
		store:    claudeaccountmodels.Default(),
		now:      time.Now,
		failedAt: make(map[string]time.Time),
	}
	l.fetch = l.executorFetch
	l.credentials = l.serviceCredentials
	l.register = func(auth *coreauth.Auth) {
		if l.s != nil {
			l.s.refreshModelRegistrationForAuth(auth)
		}
	}
	return l
}

// devyreStartClaudeAccountModels installs the learner for the request paths and
// asks Anthropic once for the credentials loaded at start.
func (s *Service) devyreStartClaudeAccountModels() {
	if s == nil || s.coreManager == nil {
		return
	}
	learner := newDevyreClaudeModelLearner(s)
	claudeaccountmodels.SetLearner(learner)
	go learner.Ensure(context.Background())
}

// devyreWithAccountClaudeModels adds to a Claude credential's catalog models
// every model Anthropic lists for its account that the catalog lacks, described
// by Anthropic's entry. The catalog's models stay: its ids include dated and
// older names Claude clients still send, which Anthropic's list may name
// differently, so the list only adds. Before Anthropic has answered, the
// catalog stands alone.
func devyreWithAccountClaudeModels(authID string, models []*ModelInfo) []*ModelInfo {
	learned, ok := claudeaccountmodels.Default().Get(authID)
	if !ok || len(learned.Entries) == 0 {
		return models
	}
	have := make(map[string]struct{}, len(models)+len(learned.Entries))
	for _, model := range models {
		if model != nil {
			have[strings.ToLower(model.ID)] = struct{}{}
		}
	}
	for _, info := range claudeaccountmodels.ModelInfos(learned) {
		key := strings.ToLower(info.ID)
		if _, dup := have[key]; dup {
			continue
		}
		have[key] = struct{}{}
		models = append(models, info)
	}
	return models
}

func (l *devyreClaudeModelLearner) Ensure(ctx context.Context) {
	credentials := l.credentials()
	if len(credentials) == 0 {
		return
	}
	now := l.now()
	missing, stale := false, false
	for _, auth := range credentials {
		learned, ok := l.store.Get(auth.ID)
		switch {
		case !ok:
			if !l.recentlyFailed(auth.ID, now) {
				missing = true
			}
		case now.Sub(learned.At) >= devyreClaudeModelsTTL:
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

func (l *devyreClaudeModelLearner) Resolve(ctx context.Context, model string) bool {
	if strings.TrimSpace(model) == "" || len(l.credentials()) == 0 {
		return false
	}
	l.mu.Lock()
	done := l.running
	recent := !l.lastEnded.IsZero() && l.now().Sub(l.lastEnded) < devyreClaudeModelsRetryGap
	l.mu.Unlock()
	if done == nil {
		if recent {
			return false
		}
		done = l.start()
	}
	return l.wait(ctx, done)
}

func (l *devyreClaudeModelLearner) recentlyFailed(authID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.failedAt[authID]
	return ok && now.Sub(at) < devyreClaudeModelsRetryGap
}

// start begins a lookup unless one already runs, and returns the channel closed
// when the lookup ends.
func (l *devyreClaudeModelLearner) start() chan struct{} {
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
		l.mu.Unlock()
		close(done)
	}()
	return done
}

// wait waits for done, at most devyreClaudeModelsWait and while ctx lives, and
// reports whether the lookup ended.
func (l *devyreClaudeModelLearner) wait(ctx context.Context, done <-chan struct{}) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(devyreClaudeModelsWait)
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

func (l *devyreClaudeModelLearner) run() {
	credentials := l.credentials()
	keep := make(map[string]struct{}, len(credentials))
	for _, auth := range credentials {
		keep[auth.ID] = struct{}{}
	}
	l.store.Retain(keep)
	if len(credentials) == 0 {
		return
	}
	work := make(chan *coreauth.Auth)
	var wg sync.WaitGroup
	workers := min(devyreClaudeModelsWorkers, len(credentials))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for auth := range work {
				l.learn(auth)
			}
		}()
	}
	for _, auth := range credentials {
		work <- auth
	}
	close(work)
	wg.Wait()
}

func (l *devyreClaudeModelLearner) learn(auth *coreauth.Auth) {
	ctx, cancel := context.WithTimeout(context.Background(), devyreClaudeModelsFetchTimeout)
	defer cancel()
	entries, errFetch := l.fetchAll(ctx, auth)
	if errFetch != nil {
		l.mu.Lock()
		l.failedAt[auth.ID] = l.now()
		l.mu.Unlock()
		log.WithError(errFetch).Warnf("claude account models: could not ask Anthropic for auth=%s; keeping what it had", auth.ID)
		return
	}
	l.mu.Lock()
	delete(l.failedAt, auth.ID)
	l.mu.Unlock()
	learned := claudeaccountmodels.Learned{At: l.now(), Entries: entries}
	if !l.store.Set(auth.ID, learned) {
		return
	}
	log.Infof("claude account models: auth=%s models=%s", auth.ID, strings.Join(learned.IDs(), ","))
	l.register(auth)
}

// fetchAll reads every page of one credential's list.
func (l *devyreClaudeModelLearner) fetchAll(ctx context.Context, auth *coreauth.Auth) ([]claudeaccountmodels.Entry, error) {
	var pages []claudeaccountmodels.Page
	afterID := ""
	for range devyreClaudeModelsMaxPages {
		body, errFetch := l.fetch(ctx, auth, afterID)
		if errFetch != nil {
			return nil, errFetch
		}
		page, errParse := claudeaccountmodels.ParsePage(body)
		if errParse != nil {
			return nil, errParse
		}
		pages = append(pages, page)
		if !page.HasMore || page.LastID == "" || page.LastID == afterID {
			break
		}
		afterID = page.LastID
	}
	return claudeaccountmodels.Merge(pages...), nil
}

func (l *devyreClaudeModelLearner) executorFetch(ctx context.Context, auth *coreauth.Auth, afterID string) ([]byte, error) {
	if l.s == nil || l.s.coreManager == nil {
		return nil, errDevyreNoClaudeExecutor
	}
	executor, ok := l.s.coreManager.Executor("claude")
	if !ok || executor == nil {
		return nil, errDevyreNoClaudeExecutor
	}
	fetcher, ok := executor.(devyreClaudeModelsFetcher)
	if !ok {
		return nil, errDevyreNoClaudeExecutor
	}
	return fetcher.DevyreFetchAccountModels(ctx, auth, afterID)
}

// serviceCredentials lists the enabled Claude credentials that reach Anthropic
// itself: subscriptions (OAuth), and API keys with Anthropic's base URL. A key
// for another gateway has its own model list, which this lookup cannot know.
func (l *devyreClaudeModelLearner) serviceCredentials() []*coreauth.Auth {
	if l.s == nil || l.s.coreManager == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range l.s.coreManager.List() {
		if devyreAsksAnthropicForModels(auth) {
			out = append(out, auth)
		}
	}
	return out
}

func devyreAsksAnthropicForModels(auth *coreauth.Auth) bool {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return false
	}
	base := ""
	if auth.Attributes != nil {
		base = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if base == "" {
		return true
	}
	parsed, errParse := url.Parse(base)
	return errParse == nil && helps.IsAnthropicUpstreamURL(parsed)
}

type devyreClaudeModelsError string

func (e devyreClaudeModelsError) Error() string { return string(e) }

const errDevyreNoClaudeExecutor = devyreClaudeModelsError("claude account models: no Claude executor can ask Anthropic")
