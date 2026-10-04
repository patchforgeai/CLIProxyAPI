package management

// devyre: usage cache for the management api-call path (plan RT-3).
//
// The T3 Code hub, the panel and the idle quota poller all read the same
// provider usage endpoints, and Claude's /api/oauth/usage is tightly rate
// limited. Allowlisted usage GETs are therefore cached per credential and URL,
// coalesced while an upstream call is in flight, answered from cache when
// upstream fails, and backed off after a Claude 429. Every successful usage
// body also feeds the quota readings store that expiring-first routing reads.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// usageCacheHeader is the management request header that asks for a
	// refresh ("refresh"), and the key of the annotation added to the header
	// map of every api-call response the cache handles.
	usageCacheHeader       = "X-CPA-Usage-Cache"
	usageCacheRefreshValue = "refresh"

	// Annotation values.
	usageCacheHit     = "hit"     // fresh cached 2xx, upstream not called
	usageCacheMiss    = "miss"    // upstream called because nothing fresh was cached
	usageCacheStale   = "stale"   // expired cached 2xx, served because upstream failed or is backed off
	usageCacheBypass  = "bypass"  // upstream called because a refresh was requested
	usageCacheBackoff = "backoff" // stored Claude 429, served while backing off

	claudeProfileCacheTTL     = time.Hour
	claudeUsageBackoffInitial = 5 * time.Minute
	claudeUsageBackoffMax     = 60 * time.Minute
)

// usageEndpoint names an allowlisted upstream endpoint.
type usageEndpoint int

const (
	usageEndpointClaudeUsage usageEndpoint = iota + 1
	usageEndpointClaudeProfile
	usageEndpointCodexUsage
)

// usageTarget describes one allowlisted URL.
type usageTarget struct {
	endpoint usageEndpoint
	// canonicalURL is the production URL of the endpoint. Usage bodies are
	// parsed against it, so tests can serve the endpoint from another address.
	canonicalURL string
}

// ttl returns how long a 2xx response of the endpoint stays fresh.
func (t usageTarget) ttl(cfg config.UsageCacheConfig) time.Duration {
	switch t.endpoint {
	case usageEndpointClaudeUsage:
		return cfg.ClaudeUsageTTLDuration()
	case usageEndpointCodexUsage:
		return cfg.CodexUsageTTLDuration()
	default:
		return claudeProfileCacheTTL
	}
}

// defaultUsageTargets is the production allowlist. Matching is on the exact
// URL, so query variants and the Codex rate-limit-reset-credits endpoints,
// through which resets are redeemed, are never cached.
func defaultUsageTargets() map[string]usageTarget {
	return map[string]usageTarget{
		quotareading.ClaudeUsageURL:   {endpoint: usageEndpointClaudeUsage, canonicalURL: quotareading.ClaudeUsageURL},
		quotareading.ClaudeProfileURL: {endpoint: usageEndpointClaudeProfile, canonicalURL: quotareading.ClaudeProfileURL},
		quotareading.CodexUsageURL:    {endpoint: usageEndpointCodexUsage, canonicalURL: quotareading.CodexUsageURL},
	}
}

// usageCacheKey is the cache key of one credential and URL. Caller-supplied
// upstream headers deliberately do not take part.
func usageCacheKey(authIndex, rawURL string) string {
	return authIndex + "|" + rawURL
}

// claudeUsageBackoff returns the backoff after the level-th consecutive Claude
// usage 429: 5 minutes, doubling per level, capped at 60 minutes.
func claudeUsageBackoff(level int) time.Duration {
	backoff := claudeUsageBackoffInitial
	for i := 1; i < level && backoff < claudeUsageBackoffMax; i++ {
		backoff *= 2
	}
	return min(backoff, claudeUsageBackoffMax)
}

// usageCache holds the cached usage responses of one management handler. Keys
// need a registered credential, so the key space is bounded by the
// credentials seen times the allowlisted URLs.
type usageCache struct {
	nowFunc func() time.Time
	// targets is the URL allowlist. It is read-only after construction.
	targets map[string]usageTarget
	// readings receives the windows parsed from successful usage bodies.
	readings *quotareading.Store
	// testHookFollowerWaiting, when set, runs as a request starts waiting for
	// another request's upstream call, so tests can synchronize without sleeping.
	testHookFollowerWaiting func()

	mu      sync.Mutex
	entries map[string]*usageCacheEntry
	// lastClaudeUsage is the start of the newest upstream call to the Claude
	// usage endpoint for any credential, by any caller.
	lastClaudeUsage time.Time
}

// usageCacheEntry is the state of one key.
type usageCacheEntry struct {
	// cached is the newest 2xx response, stored without the annotation.
	cached   *apiCallResponse
	cachedAt time.Time
	// lastCall is the start of the newest upstream call.
	lastCall time.Time
	// backoffUntil and backoffLevel track Claude usage 429s; rejected is the
	// newest 429, served while backing off when nothing is cached.
	backoffUntil time.Time
	backoffLevel int
	rejected     *apiCallResponse
	// flight is the upstream call in progress that other requests wait for.
	flight *usageFlight
}

// usageFlight lets requests wait for the upstream call of another request.
type usageFlight struct {
	done chan struct{}
	// result is the response the leader returned. It is nil when the leader
	// ended without one, in which case each waiter proceeds on its own.
	result *apiCallResponse
}

func newUsageCache() *usageCache {
	return &usageCache{
		nowFunc:  time.Now,
		targets:  defaultUsageTargets(),
		readings: quotareading.Default(),
	}
}

func (c *usageCache) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

func (c *usageCache) store() *quotareading.Store {
	if c.readings != nil {
		return c.readings
	}
	return quotareading.Default()
}

// entryLocked returns the entry of key, creating it. Callers hold c.mu.
func (c *usageCache) entryLocked(key string) *usageCacheEntry {
	if c.entries == nil {
		c.entries = make(map[string]*usageCacheEntry)
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &usageCacheEntry{}
		c.entries[key] = entry
	}
	return entry
}

// noteUpstreamCallLocked records that an upstream call for req starts at now.
// Callers hold c.mu.
func (c *usageCache) noteUpstreamCallLocked(entry *usageCacheEntry, req usageRequest, now time.Time) {
	if now.After(entry.lastCall) {
		entry.lastCall = now
	}
	if req.target.endpoint == usageEndpointClaudeUsage && now.After(c.lastClaudeUsage) {
		c.lastClaudeUsage = now
	}
}

// usageRequest is one allowlisted usage GET.
type usageRequest struct {
	key      string
	target   usageTarget
	authID   string
	provider string
	// refresh asks to bypass a fresh cached response.
	refresh bool
	// source is recorded on the windows parsed from a successful body.
	source quotareading.Source
}

// usageCacheCall is one request's passage through the usage cache. A nil call
// is a pass-through: every method is a no-op and complete returns its input.
type usageCacheCall struct {
	cache *usageCache
	req   usageRequest
	// served is the response to return without calling upstream.
	served *apiCallResponse
	// observeOnly is set while the cache is disabled: the call goes upstream
	// unchanged and only feeds readings and call times.
	observeOnly bool
	// label annotates the response of the upstream call: miss or bypass.
	label string
	// flight is set while this call leads the upstream call of its key.
	flight *usageFlight
	done   bool
}

// begin decides how req is answered. The returned call either carries a
// response to serve without calling upstream (see cachedResponse) or expects
// the caller to call upstream and then report the outcome through complete or
// fail, and finally release. It returns nil, a pass-through, when ctx ends
// while waiting for another request's upstream call.
//
//   - hit: a fresh cached 2xx is served.
//   - miss: one caller leads the upstream call; others wait for its result.
//   - backoff: during a Claude usage backoff upstream is not called; the cached
//     2xx is served as stale, otherwise the stored 429.
//   - bypass: req.refresh forces an upstream call once the newest upstream
//     call for the key is at least the refresh floor old; inside the floor the
//     request is answered as if no refresh was asked.
func (c *usageCache) begin(ctx context.Context, req usageRequest, cfg config.UsageCacheConfig) *usageCacheCall {
	if ctx == nil {
		ctx = context.Background()
	}
	if !cfg.IsEnabled() {
		c.mu.Lock()
		c.noteUpstreamCallLocked(c.entryLocked(req.key), req, c.now())
		c.mu.Unlock()
		return &usageCacheCall{cache: c, req: req, observeOnly: true}
	}
	ttl, floor := req.target.ttl(cfg), cfg.RefreshFloorDuration()
	waited := false
	for {
		now := c.now()
		c.mu.Lock()
		entry := c.entryLocked(req.key)
		forced := req.refresh && (entry.lastCall.IsZero() || now.Sub(entry.lastCall) >= floor)
		fresh := entry.cached != nil && now.Sub(entry.cachedAt) < ttl
		// A backoff always has its 429 stored; both are set together in complete.
		backoff := entry.rejected != nil && now.Before(entry.backoffUntil)
		switch {
		case fresh && (!forced || backoff):
			served := entry.cached.withUsageCacheLabel(usageCacheHit)
			c.mu.Unlock()
			return &usageCacheCall{cache: c, req: req, served: &served}
		case backoff:
			served := entry.rejected.withUsageCacheLabel(usageCacheBackoff)
			if entry.cached != nil {
				served = entry.cached.withUsageCacheLabel(usageCacheStale)
			}
			c.mu.Unlock()
			return &usageCacheCall{cache: c, req: req, served: &served}
		case entry.flight != nil && !waited:
			flight := entry.flight
			hook := c.testHookFollowerWaiting
			c.mu.Unlock()
			if hook != nil {
				hook()
			}
			select {
			case <-flight.done:
			case <-ctx.Done():
				return nil
			}
			if flight.result != nil {
				served := flight.result.clone()
				return &usageCacheCall{cache: c, req: req, served: &served}
			}
			// The leader ended without a response: proceed on our own, after
			// one more look at the cache.
			waited = true
			continue
		}
		call := &usageCacheCall{cache: c, req: req, label: usageCacheMiss}
		if forced {
			call.label = usageCacheBypass
		}
		if entry.flight == nil {
			call.flight = &usageFlight{done: make(chan struct{})}
			entry.flight = call.flight
		}
		c.noteUpstreamCallLocked(entry, req, now)
		c.mu.Unlock()
		return call
	}
}

// cachedResponse returns the response to send without calling upstream.
func (call *usageCacheCall) cachedResponse() (apiCallResponse, bool) {
	if call == nil || call.served == nil {
		return apiCallResponse{}, false
	}
	return *call.served, true
}

// complete records the upstream response and returns the response to send.
// A 2xx is stored and its windows are recorded as readings. A Claude usage
// 429 starts or extends the backoff. A 429 or 5xx is answered with the cached
// 2xx marked stale when one exists; anything else passes through annotated.
func (call *usageCacheCall) complete(resp apiCallResponse) apiCallResponse {
	if call == nil || call.done || call.served != nil {
		return resp
	}
	c := call.cache
	now := c.now()
	success := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if call.observeOnly {
		call.done = true
		if success {
			c.recordReadings(call.req, resp.Body, now)
		}
		return resp
	}
	c.mu.Lock()
	entry := c.entryLocked(call.req.key)
	out := resp.withUsageCacheLabel(call.label)
	switch {
	case success:
		stored := resp.clone()
		entry.cached, entry.cachedAt = &stored, now
		entry.backoffUntil, entry.backoffLevel, entry.rejected = time.Time{}, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests && call.req.target.endpoint == usageEndpointClaudeUsage:
		// Only a call made after the previous backoff ended escalates it; a
		// 429 from a call that overlapped the one starting it does not.
		if !now.Before(entry.backoffUntil) {
			entry.backoffLevel++
			entry.backoffUntil = now.Add(claudeUsageBackoff(entry.backoffLevel))
		}
		rejected := resp.clone()
		entry.rejected = &rejected
		if entry.cached != nil {
			out = entry.cached.withUsageCacheLabel(usageCacheStale)
		}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		if entry.cached != nil {
			out = entry.cached.withUsageCacheLabel(usageCacheStale)
		}
	}
	call.finishLocked(entry, &out)
	c.mu.Unlock()
	if success {
		c.recordReadings(call.req, resp.Body, now)
	}
	return out
}

// fail records a transport error. It returns the cached 2xx marked stale, or
// nil when nothing is cached and the caller should report its own error.
func (call *usageCacheCall) fail() *apiCallResponse {
	if call == nil || call.done || call.served != nil || call.observeOnly {
		return nil
	}
	c := call.cache
	c.mu.Lock()
	entry := c.entryLocked(call.req.key)
	var out *apiCallResponse
	if entry.cached != nil {
		stale := entry.cached.withUsageCacheLabel(usageCacheStale)
		out = &stale
	}
	call.finishLocked(entry, out)
	c.mu.Unlock()
	return out
}

// release ends the call. Deferred right after begin, it frees the requests
// waiting on this call on every exit path, including early error returns;
// without a result they proceed on their own.
func (call *usageCacheCall) release() {
	if call == nil || call.done || call.served != nil || call.observeOnly {
		return
	}
	c := call.cache
	c.mu.Lock()
	call.finishLocked(c.entryLocked(call.req.key), nil)
	c.mu.Unlock()
}

// finishLocked marks the call done and hands result to the requests waiting
// on its upstream call. Callers hold the cache mutex.
func (call *usageCacheCall) finishLocked(entry *usageCacheEntry, result *apiCallResponse) {
	call.done = true
	flight := call.flight
	if flight == nil {
		return
	}
	call.flight = nil
	if entry.flight == flight {
		entry.flight = nil
	}
	if result != nil {
		shared := result.clone()
		flight.result = &shared
	}
	close(flight.done)
}

// recordReadings feeds the windows of a successful usage body to the readings
// store. Profile bodies carry no windows. Parse errors never include the body.
func (c *usageCache) recordReadings(req usageRequest, body string, now time.Time) {
	windows, errParse := quotareading.FromUsageBody(req.provider, req.target.canonicalURL, []byte(body), now)
	if errParse != nil {
		log.WithError(errParse).WithField("provider", req.provider).Debug("usage cache: usage body not recorded")
		return
	}
	if len(windows) == 0 {
		return
	}
	if req.source != "" {
		for i := range windows {
			windows[i].Source = req.source
		}
	}
	c.store().Put(req.authID, req.provider, windows)
}

// clone returns a deep copy so cached responses never share header slices.
func (r apiCallResponse) clone() apiCallResponse {
	if r.Header != nil {
		header := make(map[string][]string, len(r.Header)+1)
		for key, values := range r.Header {
			header[key] = append([]string(nil), values...)
		}
		r.Header = header
	}
	return r
}

// withUsageCacheLabel returns a copy of r annotated with how the cache answered.
func (r apiCallResponse) withUsageCacheLabel(label string) apiCallResponse {
	out := r.clone()
	if out.Header == nil {
		out.Header = make(map[string][]string, 1)
	}
	out.Header[usageCacheHeader] = []string{label}
	return out
}

// usageCacheLabel returns the cache annotation of resp, or "".
func usageCacheLabel(resp apiCallResponse) string {
	if values := resp.Header[usageCacheHeader]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// devyreHandlerState is the fork's per-handler state. It lives in a side table
// instead of a Handler field so the upstream struct stays untouched.
type devyreHandlerState struct {
	usage *usageCache
}

// devyreStates maps *Handler to *devyreHandlerState. A server owns one
// handler for the life of the process, so entries are never removed.
var devyreStates sync.Map

func (h *Handler) devyreState() *devyreHandlerState {
	if state, ok := devyreStates.Load(h); ok {
		return state.(*devyreHandlerState)
	}
	state, _ := devyreStates.LoadOrStore(h, &devyreHandlerState{usage: newUsageCache()})
	return state.(*devyreHandlerState)
}

func (h *Handler) devyreUsageCache() *usageCache {
	return h.devyreState().usage
}

// devyreRoutingConfig returns a copy of the current routing settings. The
// config pointer is replaced on hot reload and edited in place by management
// writes, both under h.mu.
func (h *Handler) devyreRoutingConfig() config.RoutingConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return config.RoutingConfig{}
	}
	return h.cfg.Routing
}

// beginUsageCall routes an api-call through the usage cache when it is a GET
// of an allowlisted URL for a registered credential, and returns nil for every
// other call. A request header X-CPA-Usage-Cache: refresh asks for a bypass.
func (h *Handler) beginUsageCall(c *gin.Context, method, rawURL string, auth *coreauth.Auth) *usageCacheCall {
	if h == nil || c == nil || c.Request == nil || method != http.MethodGet || auth == nil {
		return nil
	}
	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		return nil
	}
	cache := h.devyreUsageCache()
	target, ok := cache.targets[rawURL]
	if !ok {
		return nil
	}
	return cache.begin(c.Request.Context(), usageRequest{
		key:      usageCacheKey(authIndex, rawURL),
		target:   target,
		authID:   auth.ID,
		provider: auth.Provider,
		refresh:  strings.EqualFold(strings.TrimSpace(c.GetHeader(usageCacheHeader)), usageCacheRefreshValue),
		source:   quotareading.SourceUsage,
	}, h.devyreRoutingConfig().QuotaObservation.UsageCache)
}
