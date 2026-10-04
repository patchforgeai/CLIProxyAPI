package management

// devyre: idle-credential quota poller (plan RT-4).
//
// Response headers keep the readings of busy credentials current. Credentials
// without traffic only get readings when someone asks for their usage, so this
// loop polls them through the usage cache, which also warms the cache for the
// panel and the T3 Code hub.

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	quotaObserverTick = time.Minute

	claudeUsageBetaHeader = "oauth-2025-04-20"
)

// StartQuotaObserver starts, once per handler, the background loop that polls
// the usage endpoints of idle Claude and Codex OAuth credentials. Every minute
// it re-reads the handler's current config and does nothing unless the poller
// is enabled: explicitly through routing.quota-observation.poller.enabled, or
// by default when the routing strategy is expiring-first.
func (h *Handler) StartQuotaObserver(ctx context.Context) {
	if h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.devyreState().observerOnce.Do(func() {
		go h.runQuotaObserver(ctx)
	})
}

func (h *Handler) runQuotaObserver(ctx context.Context) {
	ticker := time.NewTicker(quotaObserverTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.observeQuotaOnce(ctx)
		}
	}
}

// observeQuotaOnce runs one poller tick: it picks the due credentials with
// nextPolls and polls them one after another.
func (h *Handler) observeQuotaOnce(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.WithField("panic", recovered).Error("quota observer: tick panicked")
		}
	}()
	routing := h.devyreRoutingConfig()
	if !routing.QuotaPollerEnabled() {
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return
	}
	cache := h.devyreUsageCache()
	now := cache.now()
	candidates, polls := cache.quotaPollCandidates(manager.List(), now)
	for _, authID := range nextPolls(now, candidates, cache.lastClaudeUsageCall(), routing.QuotaObservation.Poller) {
		if ctx.Err() != nil {
			return
		}
		h.pollUsage(ctx, cache, polls[authID], routing.QuotaObservation.UsageCache)
	}
}

// quotaPollCandidate is the scheduling view of one credential.
type quotaPollCandidate struct {
	authID   string
	provider string
	disabled bool
	oauth    bool
	// newestLong is the newest observation of any long window, zero when none.
	newestLong time.Time
	// lastCall is the start of the newest upstream call to the credential's
	// usage URL by any caller, zero when none.
	lastCall  time.Time
	inBackoff bool
}

// quotaPoll is what the poller needs to query one credential.
type quotaPoll struct {
	auth   *coreauth.Auth
	url    string
	target usageTarget
}

// pollableUsageEndpoint maps a provider to the usage endpoint the poller
// queries. Providers without one (xAI, Meta, Kimi, Antigravity, ...) are
// never polled.
func pollableUsageEndpoint(provider string) (usageEndpoint, bool) {
	switch provider {
	case "claude":
		return usageEndpointClaudeUsage, true
	case "codex":
		return usageEndpointCodexUsage, true
	default:
		return 0, false
	}
}

// quotaPollCandidates builds the scheduling view of auths at now, keyed for
// nextPolls by auth ID.
func (c *usageCache) quotaPollCandidates(auths []*coreauth.Auth, now time.Time) ([]quotaPollCandidate, map[string]quotaPoll) {
	candidates := make([]quotaPollCandidate, 0, len(auths))
	polls := make(map[string]quotaPoll, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		endpoint, ok := pollableUsageEndpoint(provider)
		if !ok {
			continue
		}
		rawURL, target, ok := c.urlFor(endpoint)
		if !ok {
			continue
		}
		authIndex := auth.EnsureIndex()
		if authIndex == "" {
			continue
		}
		reading := quotareading.Effective(c.store(), auth.ID, provider, auth.Quota.Signals, auth.Quota.ObservedAt, now)
		lastCall, inBackoff := c.keyState(usageCacheKey(authIndex, rawURL), now)
		candidates = append(candidates, quotaPollCandidate{
			authID:     auth.ID,
			provider:   provider,
			disabled:   auth.Disabled || auth.Status == coreauth.StatusDisabled,
			oauth:      auth.AuthKind() == coreauth.AuthKindOAuth,
			newestLong: newestLongObservation(reading),
			lastCall:   lastCall,
			inBackoff:  inBackoff,
		})
		polls[auth.ID] = quotaPoll{auth: auth, url: rawURL, target: target}
	}
	return candidates, polls
}

// newestLongObservation returns the newest ObservedAt among the long windows
// of r, or zero.
func newestLongObservation(r quotareading.Reading) time.Time {
	var newest time.Time
	for _, window := range r.Windows {
		if window.Kind == quotareading.KindLong && window.ObservedAt.After(newest) {
			newest = window.ObservedAt
		}
	}
	return newest
}

// nextPolls returns, stalest first, the auth IDs to poll at now. A candidate
// is due when it is an enabled OAuth credential of provider claude or codex,
// its newest long-window reading is missing or at least the provider interval
// old, its usage URL was not called upstream within that interval (so a
// failing credential is retried once per interval, not every tick), and it is
// not in usage-cache backoff. Claude polls also need the newest Claude usage
// call by anyone to be at least claude-min-gap old, and at most one Claude
// credential is polled per tick. The Claude pick comes first.
func nextPolls(now time.Time, candidates []quotaPollCandidate, lastClaudeCall time.Time, cfg config.QuotaPollerConfig) []string {
	var claude, codex []quotaPollCandidate
	for _, candidate := range candidates {
		if candidate.authID == "" || candidate.disabled || !candidate.oauth || candidate.inBackoff {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(candidate.provider))
		var interval time.Duration
		switch provider {
		case "claude":
			interval = cfg.ClaudeIntervalDuration()
		case "codex":
			interval = cfg.CodexIntervalDuration()
		default:
			continue
		}
		if !atLeastAgo(now, candidate.newestLong, interval) || !atLeastAgo(now, candidate.lastCall, interval) {
			continue
		}
		if provider == "claude" {
			claude = append(claude, candidate)
		} else {
			codex = append(codex, candidate)
		}
	}
	sortStalestFirst(claude)
	sortStalestFirst(codex)
	due := make([]string, 0, 1+len(codex))
	if len(claude) > 0 && atLeastAgo(now, lastClaudeCall, cfg.ClaudeMinGapDuration()) {
		due = append(due, claude[0].authID)
	}
	for _, candidate := range codex {
		due = append(due, candidate.authID)
	}
	return due
}

// atLeastAgo reports whether t is unknown (zero) or at least d before now.
func atLeastAgo(now, t time.Time, d time.Duration) bool {
	return t.IsZero() || now.Sub(t) >= d
}

// sortStalestFirst orders candidates by oldest long-window reading (missing
// first), then oldest upstream call, then auth ID.
func sortStalestFirst(candidates []quotaPollCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.newestLong.Equal(b.newestLong) {
			return a.newestLong.Before(b.newestLong)
		}
		if !a.lastCall.Equal(b.lastCall) {
			return a.lastCall.Before(b.lastCall)
		}
		return a.authID < b.authID
	})
}

// pollUsage queries one credential's usage endpoint through the usage cache,
// so the response is coalesced with and shared by concurrent api-calls, and
// its windows are recorded with source "poll".
func (h *Handler) pollUsage(ctx context.Context, cache *usageCache, poll quotaPoll, cfg config.UsageCacheConfig) {
	auth := poll.auth
	if auth == nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	logEntry := log.WithFields(log.Fields{"provider": provider, "auth_index": auth.Index})
	call := cache.begin(ctx, usageRequest{
		key:      usageCacheKey(auth.Index, poll.url),
		target:   poll.target,
		authID:   auth.ID,
		provider: provider,
		source:   quotareading.SourcePoll,
	}, cfg)
	if call == nil {
		return
	}
	if served, ok := call.cachedResponse(); ok {
		logEntry.WithField("cache", usageCacheLabel(served)).Debug("quota observer: answered by usage cache")
		return
	}
	defer call.release()

	token, errToken := h.resolveTokenForAuth(ctx, auth, "")
	if errToken != nil || token == "" {
		logEntry.Debug("quota observer: credential has no usable token")
		return
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, poll.url, nil)
	if errRequest != nil {
		logEntry.WithError(errRequest).Debug("quota observer: build usage request")
		return
	}
	setQuotaPollHeaders(req.Header, auth, poll.target.endpoint, token)
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth, ""),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		call.fail()
		logEntry.WithError(errDo).Debug("quota observer: usage request failed")
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("quota observer: close usage response body")
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		call.fail()
		logEntry.WithError(errRead).Debug("quota observer: read usage response")
		return
	}
	out := call.complete(apiCallResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: string(body)})
	logEntry.WithFields(log.Fields{"status": resp.StatusCode, "cache": usageCacheLabel(out)}).Debug("quota observer: polled usage")
}

// setQuotaPollHeaders sets the headers the panel and the T3 Code hub send to
// the endpoint.
func setQuotaPollHeaders(header http.Header, auth *coreauth.Auth, endpoint usageEndpoint, token string) {
	header.Set("Authorization", "Bearer "+token)
	switch endpoint {
	case usageEndpointClaudeUsage:
		header.Set("anthropic-beta", claudeUsageBetaHeader)
	case usageEndpointCodexUsage:
		header.Set("Content-Type", "application/json")
		header.Set("OpenAI-Beta", "codex-1")
		header.Set("Originator", "Codex Desktop")
		if accountID := codexChatGPTAccountID(auth); accountID != "" {
			header.Set("Chatgpt-Account-Id", accountID)
		}
	}
}

// codexChatGPTAccountID returns id_token.chatgpt_account_id, the value the
// auth-files listing exposes, falling back to metadata account_id, which the
// Codex executor sends.
func codexChatGPTAccountID(auth *coreauth.Auth) string {
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		if accountID, ok := claims["chatgpt_account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			return strings.TrimSpace(accountID)
		}
	}
	if auth == nil {
		return ""
	}
	return stringValue(auth.Metadata, "account_id")
}

// lastClaudeUsageCall returns the start of the newest Claude usage upstream
// call by any caller, or zero.
func (c *usageCache) lastClaudeUsageCall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastClaudeUsage
}

// keyState returns the start of the newest upstream call for key and whether
// a Claude usage backoff is active at now.
func (c *usageCache) keyState(key string, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return time.Time{}, false
	}
	return entry.lastCall, now.Before(entry.backoffUntil)
}

// urlFor returns the allowlisted URL of endpoint. Production has exactly one;
// with several, the smallest URL wins so the choice is stable.
func (c *usageCache) urlFor(endpoint usageEndpoint) (string, usageTarget, bool) {
	urls := make([]string, 0, 1)
	for rawURL, target := range c.targets {
		if target.endpoint == endpoint {
			urls = append(urls, rawURL)
		}
	}
	if len(urls) == 0 {
		return "", usageTarget{}, false
	}
	sort.Strings(urls)
	return urls[0], c.targets[urls[0]], true
}
