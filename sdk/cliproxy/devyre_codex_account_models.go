package cliproxy

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexaccountmodels"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// devyre: the models OpenAI offers each Codex account, asked of OpenAI with the
// account's own credential (internal/codexaccountmodels has the why). A lookup
// asks every Codex OAuth credential at once, one lookup at a time:
//
//   - when a Codex client asks for its model list (/v1/models?client_version=),
//     for that client's version; the list waits for credentials that have no
//     answer for it yet, and stale answers are refreshed behind it;
//   - when a request names a model no credential serves: it waits for a running
//     lookup, or starts one unless one ended within devyreCodexModelsRetryGap;
//   - once at start.
//
// A credential whose lookup fails keeps what it had. What changed is registered
// again at once, so routing and the model lists follow without a restart.
const (
	// devyreCodexModelsFetchTimeout bounds one credential's lookup.
	devyreCodexModelsFetchTimeout = 15 * time.Second
	// devyreCodexModelsWait is the longest a model list or a request waits.
	devyreCodexModelsWait = 10 * time.Second
	// devyreCodexModelsTTL is how long an answer stands before it is refreshed.
	devyreCodexModelsTTL = 15 * time.Minute
	// devyreCodexModelsRetryGap spaces the lookups an unknown model starts.
	devyreCodexModelsRetryGap = time.Minute
	// devyreCodexModelsWorkers bounds the credentials asked at once.
	devyreCodexModelsWorkers = 4
)

// devyreCodexModelsFetcher is the Codex executor's lookup
// (executor.CodexExecutor.DevyreFetchAccountModels).
type devyreCodexModelsFetcher interface {
	DevyreFetchAccountModels(ctx context.Context, auth *coreauth.Auth, clientVersion string) ([]byte, error)
}

type devyreCodexModelLearner struct {
	s     *Service
	store *codexaccountmodels.Store
	now   func() time.Time
	// fetch asks OpenAI for one credential (the executor's; tests replace it).
	fetch func(ctx context.Context, auth *coreauth.Auth, clientVersion string) ([]byte, error)
	// credentials lists the Codex OAuth credentials (tests replace it).
	credentials func() []*coreauth.Auth
	// register registers a credential's models again (tests replace it).
	register func(auth *coreauth.Auth)

	mu sync.Mutex
	// version is the newest Codex client version asked for.
	version string
	// running is closed when the running lookup ends; nil when none runs.
	running chan struct{}
	// runningVersion is the version the running lookup asks for.
	runningVersion string
	// lastEnded is when the last lookup that asked a credential ended.
	lastEnded time.Time
	// failedAt is when each credential's last lookup failed.
	failedAt map[string]time.Time
}

func newDevyreCodexModelLearner(s *Service) *devyreCodexModelLearner {
	l := &devyreCodexModelLearner{
		s:        s,
		store:    codexaccountmodels.Default(),
		now:      time.Now,
		version:  codexaccountmodels.FloorClientVersion,
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

// devyreStartCodexAccountModels installs the learner for the request paths and
// asks OpenAI once for the credentials loaded at start.
func (s *Service) devyreStartCodexAccountModels() {
	if s == nil || s.coreManager == nil {
		return
	}
	learner := newDevyreCodexModelLearner(s)
	codexaccountmodels.SetLearner(learner)
	go learner.Ensure(context.Background(), "")
}

// devyreWithAccountCodexModels adds to a Codex OAuth credential's catalog
// models the models OpenAI lists for its account that the catalog lacks.
func devyreWithAccountCodexModels(authID string, models []*ModelInfo) []*ModelInfo {
	learned, ok := codexaccountmodels.Default().Get(authID)
	if !ok || len(learned.Entries) == 0 {
		return models
	}
	have := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			have[strings.ToLower(model.ID)] = struct{}{}
		}
	}
	for _, info := range codexaccountmodels.ModelInfos(learned) {
		key := strings.ToLower(info.ID)
		if _, dup := have[key]; dup {
			continue
		}
		have[key] = struct{}{}
		models = append(models, info)
	}
	return models
}

func (l *devyreCodexModelLearner) Ensure(ctx context.Context, clientVersion string) {
	version := l.noteVersion(clientVersion)
	credentials := l.credentials()
	if len(credentials) == 0 {
		return
	}
	now := l.now()
	missing, stale := false, false
	for _, auth := range credentials {
		learned, ok := l.store.Get(auth.ID)
		switch {
		case !ok || codexaccountmodels.CompareVersions(learned.ClientVersion, version) < 0:
			if !l.recentlyFailed(auth.ID, now) {
				missing = true
			}
		case now.Sub(learned.At) >= devyreCodexModelsTTL:
			stale = true
		}
	}
	if !missing && !stale {
		return
	}
	done := l.start(version)
	if missing {
		l.wait(ctx, done)
	}
}

func (l *devyreCodexModelLearner) Resolve(ctx context.Context, model string) bool {
	if strings.TrimSpace(model) == "" || len(l.credentials()) == 0 {
		return false
	}
	l.mu.Lock()
	done := l.running
	recent := !l.lastEnded.IsZero() && l.now().Sub(l.lastEnded) < devyreCodexModelsRetryGap
	version := l.version
	l.mu.Unlock()
	if done == nil {
		if recent {
			return false
		}
		done = l.start(version)
	}
	return l.wait(ctx, done)
}

// noteVersion keeps the newest client version named and returns it.
func (l *devyreCodexModelLearner) noteVersion(clientVersion string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	clientVersion = strings.TrimSpace(clientVersion)
	if codexaccountmodels.ValidVersion(clientVersion) && codexaccountmodels.CompareVersions(clientVersion, l.version) > 0 {
		l.version = clientVersion
	}
	return l.version
}

func (l *devyreCodexModelLearner) recentlyFailed(authID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.failedAt[authID]
	return ok && now.Sub(at) < devyreCodexModelsRetryGap
}

// start begins a lookup for version unless one already runs for it or a newer
// one, and returns the channel closed when the lookup that serves it ends.
func (l *devyreCodexModelLearner) start(version string) chan struct{} {
	l.mu.Lock()
	if l.running != nil && codexaccountmodels.CompareVersions(l.runningVersion, version) >= 0 {
		done := l.running
		l.mu.Unlock()
		return done
	}
	previous := l.running
	done := make(chan struct{})
	l.running = done
	l.runningVersion = version
	l.mu.Unlock()
	go func() {
		if previous != nil {
			// One lookup at a time: a newer version waits for the older one.
			<-previous
		}
		l.run(version)
		l.mu.Lock()
		if l.running == done {
			l.running = nil
			l.runningVersion = ""
		}
		l.mu.Unlock()
		close(done)
	}()
	return done
}

// wait waits for done, at most devyreCodexModelsWait and while ctx lives, and
// reports whether the lookup ended.
func (l *devyreCodexModelLearner) wait(ctx context.Context, done <-chan struct{}) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(devyreCodexModelsWait)
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

func (l *devyreCodexModelLearner) run(version string) {
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
	workers := min(devyreCodexModelsWorkers, len(credentials))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for auth := range work {
				l.learn(auth, version)
			}
		}()
	}
	for _, auth := range credentials {
		work <- auth
	}
	close(work)
	wg.Wait()
	l.mu.Lock()
	l.lastEnded = l.now()
	l.mu.Unlock()
}

func (l *devyreCodexModelLearner) learn(auth *coreauth.Auth, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), devyreCodexModelsFetchTimeout)
	defer cancel()
	body, errFetch := l.fetch(ctx, auth, version)
	var entries []map[string]any
	if errFetch == nil {
		entries, errFetch = codexaccountmodels.ParseCatalog(body)
	}
	if errFetch != nil {
		l.mu.Lock()
		l.failedAt[auth.ID] = l.now()
		l.mu.Unlock()
		log.WithError(errFetch).Warnf("codex account models: could not ask OpenAI for auth=%s; keeping what it had", auth.ID)
		return
	}
	l.mu.Lock()
	delete(l.failedAt, auth.ID)
	l.mu.Unlock()
	learned := codexaccountmodels.Learned{ClientVersion: version, At: l.now(), Entries: entries}
	if !l.store.Set(auth.ID, learned) {
		return
	}
	log.Infof("codex account models: auth=%s client_version=%s models=%s", auth.ID, version, strings.Join(learned.Slugs(), ","))
	l.register(auth)
}

func (l *devyreCodexModelLearner) executorFetch(ctx context.Context, auth *coreauth.Auth, clientVersion string) ([]byte, error) {
	if l.s == nil || l.s.coreManager == nil {
		return nil, errDevyreNoCodexExecutor
	}
	executor, ok := l.s.coreManager.Executor("codex")
	if !ok || executor == nil {
		return nil, errDevyreNoCodexExecutor
	}
	fetcher, ok := executor.(devyreCodexModelsFetcher)
	if !ok {
		return nil, errDevyreNoCodexExecutor
	}
	return fetcher.DevyreFetchAccountModels(ctx, auth, clientVersion)
}

// serviceCredentials lists the enabled Codex OAuth credentials: an API key has
// no ChatGPT account to ask.
func (l *devyreCodexModelLearner) serviceCredentials() []*coreauth.Auth {
	if l.s == nil || l.s.coreManager == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range l.s.coreManager.List() {
		if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		if auth.AuthKind() == coreauth.AuthKindAPIKey {
			continue
		}
		out = append(out, auth)
	}
	return out
}

type devyreCodexModelsError string

func (e devyreCodexModelsError) Error() string { return string(e) }

const errDevyreNoCodexExecutor = devyreCodexModelsError("codex account models: no Codex executor can ask OpenAI")
