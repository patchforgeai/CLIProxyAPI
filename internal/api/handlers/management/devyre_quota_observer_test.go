package management

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// pollCandidate builds an enabled OAuth candidate with no readings and no calls.
func pollCandidate(id, provider string, edits ...func(*quotaPollCandidate)) quotaPollCandidate {
	candidate := quotaPollCandidate{authID: id, provider: provider, oauth: true}
	for _, edit := range edits {
		edit(&candidate)
	}
	return candidate
}

func readAt(at time.Time) func(*quotaPollCandidate) {
	return func(c *quotaPollCandidate) { c.newestLong = at }
}

func calledAt(at time.Time) func(*quotaPollCandidate) {
	return func(c *quotaPollCandidate) { c.lastCall = at }
}

func TestNextPolls(t *testing.T) {
	t.Parallel()
	now := usageTestStart
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	disabled := func(c *quotaPollCandidate) { c.disabled = true }
	backoff := func(c *quotaPollCandidate) { c.inBackoff = true }
	apiKey := func(c *quotaPollCandidate) { c.oauth = false }
	cases := []struct {
		name       string
		candidates []quotaPollCandidate
		lastClaude time.Time
		cfg        config.QuotaPollerConfig
		want       []string
	}{
		{
			name:       "missing readings are due: one claude per tick, every codex",
			candidates: []quotaPollCandidate{pollCandidate("claude-b", "claude"), pollCandidate("codex-d", "codex"), pollCandidate("claude-a", "claude"), pollCandidate("codex-c", "codex")},
			want:       []string{"claude-a", "codex-c", "codex-d"},
		},
		{
			name: "readings fresher than the interval are not due",
			candidates: []quotaPollCandidate{
				pollCandidate("claude-a", "claude", readAt(ago(30*time.Minute-time.Second))),
				pollCandidate("codex-c", "codex", readAt(ago(5*time.Minute-time.Second))),
			},
		},
		{
			name: "readings as old as the interval are due",
			candidates: []quotaPollCandidate{
				pollCandidate("claude-a", "claude", readAt(ago(30*time.Minute))),
				pollCandidate("codex-c", "codex", readAt(ago(5*time.Minute))),
			},
			want: []string{"claude-a", "codex-c"},
		},
		{
			name: "the stalest claude reading goes first, a missing one before all",
			candidates: []quotaPollCandidate{
				pollCandidate("claude-a", "claude", readAt(ago(31*time.Minute))),
				pollCandidate("claude-b", "claude", readAt(ago(2*time.Hour))),
				pollCandidate("claude-c", "claude"),
			},
			want: []string{"claude-c"},
		},
		{
			name: "equal readings: the oldest call goes first, then the auth ID",
			candidates: []quotaPollCandidate{
				pollCandidate("claude-a", "claude", calledAt(ago(40*time.Minute))),
				pollCandidate("claude-b", "claude", calledAt(ago(50*time.Minute))),
				pollCandidate("codex-d", "codex", calledAt(ago(6*time.Minute))),
				pollCandidate("codex-e", "codex", calledAt(ago(6*time.Minute))),
				pollCandidate("codex-c", "codex", calledAt(ago(5*time.Minute))),
			},
			want: []string{"claude-b", "codex-d", "codex-e", "codex-c"},
		},
		{
			name: "a usage call inside the interval holds off a credential without readings",
			candidates: []quotaPollCandidate{
				pollCandidate("claude-a", "claude", calledAt(ago(30*time.Minute-time.Second))),
				pollCandidate("codex-c", "codex", calledAt(ago(5*time.Minute-time.Second))),
			},
		},
		{
			name:       "claude waits for the min gap since any claude usage call",
			candidates: []quotaPollCandidate{pollCandidate("claude-a", "claude"), pollCandidate("codex-c", "codex")},
			lastClaude: ago(10*time.Minute - time.Second),
			want:       []string{"codex-c"},
		},
		{
			name:       "claude polls once the min gap has passed",
			candidates: []quotaPollCandidate{pollCandidate("claude-a", "claude")},
			lastClaude: ago(10 * time.Minute),
			want:       []string{"claude-a"},
		},
		{
			name:       "credentials in usage-cache backoff are skipped",
			candidates: []quotaPollCandidate{pollCandidate("claude-a", "claude", backoff), pollCandidate("claude-b", "claude"), pollCandidate("codex-c", "codex", backoff)},
			want:       []string{"claude-b"},
		},
		{
			name:       "disabled credentials are never polled",
			candidates: []quotaPollCandidate{pollCandidate("claude-a", "claude", disabled), pollCandidate("codex-c", "codex", disabled)},
		},
		{
			name:       "API key credentials are never polled",
			candidates: []quotaPollCandidate{pollCandidate("claude-a", "claude", apiKey), pollCandidate("codex-c", "codex", apiKey)},
		},
		{
			name: "other providers are never polled",
			candidates: []quotaPollCandidate{
				pollCandidate("xai-a", "xai"), pollCandidate("meta-a", "meta"), pollCandidate("kimi-a", "kimi"),
				pollCandidate("antigravity-a", "antigravity"), pollCandidate("gemini-a", "gemini"), pollCandidate("", "claude"),
			},
		},
		{
			name:       "provider names are matched case-insensitively",
			candidates: []quotaPollCandidate{pollCandidate("claude-a", " Claude "), pollCandidate("codex-c", "CODEX")},
			want:       []string{"claude-a", "codex-c"},
		},
		{
			name: "configured intervals and gap",
			candidates: []quotaPollCandidate{
				pollCandidate("claude-a", "claude", readAt(ago(59*time.Minute))),
				pollCandidate("claude-b", "claude", readAt(ago(time.Hour))),
				pollCandidate("codex-c", "codex", readAt(ago(time.Minute))),
				pollCandidate("codex-d", "codex", readAt(ago(59*time.Second))),
			},
			lastClaude: ago(time.Minute),
			cfg:        config.QuotaPollerConfig{ClaudeInterval: "1h", ClaudeMinGap: "1m", CodexInterval: "1m"},
			want:       []string{"claude-b", "codex-c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nextPolls(now, tc.candidates, tc.lastClaude, tc.cfg)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("nextPolls = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaPollCandidatesUseEffectiveReadings(t *testing.T) {
	t.Parallel()
	clock := newUsageTestClock()
	store := quotareading.NewStore()
	cache := &usageCache{nowFunc: clock.Now, targets: usageTestTargets("http://upstream.test"), readings: store}
	headerAt := usageTestStart.Add(-20 * time.Minute)
	busy := usageTestClaudeAuth("claude-busy.json", "busy-token")
	busy.Quota.ObservedAt = headerAt
	busy.Quota.Signals = map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(usageTestStart.Add(48*time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
	}
	polled := usageTestClaudeAuth("claude-polled.json", "polled-token")
	pollAt := usageTestStart.Add(-5 * time.Minute)
	store.Put(polled.ID, "claude", []quotareading.Window{
		{ID: "7d", Kind: quotareading.KindLong, UsedPercent: 10, Length: 7 * 24 * time.Hour, ObservedAt: pollAt, Source: quotareading.SourcePoll},
		{ID: "5h", Kind: quotareading.KindShort, UsedPercent: 10, Length: 5 * time.Hour, ObservedAt: usageTestStart, Source: quotareading.SourcePoll},
	})
	shortOnly := usageTestCodexAuth("codex-short.json", "codex-token", "acc")
	store.Put(shortOnly.ID, "codex", []quotareading.Window{
		{ID: "primary", Kind: quotareading.KindShort, UsedPercent: 5, ObservedAt: usageTestStart, Source: quotareading.SourcePoll},
	})
	disabled := usageTestClaudeAuth("claude-disabled.json", "disabled-token")
	disabled.Status = coreauth.StatusDisabled
	apiKey := &coreauth.Auth{ID: "claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "sk-test"}}
	xai := &coreauth.Auth{ID: "xai.json", Provider: "xai", Metadata: map[string]any{"access_token": "xai-token"}}

	candidates, polls := cache.quotaPollCandidates([]*coreauth.Auth{busy, polled, shortOnly, disabled, apiKey, xai, nil}, clock.Now())
	got := make(map[string]quotaPollCandidate, len(candidates))
	for _, candidate := range candidates {
		got[candidate.authID] = candidate
	}
	if len(got) != 5 {
		t.Fatalf("candidates = %+v, want every Claude and Codex credential and no others", candidates)
	}
	if !got[busy.ID].newestLong.Equal(headerAt) {
		t.Fatalf("busy newestLong = %s, want the header observation %s", got[busy.ID].newestLong, headerAt)
	}
	if !got[polled.ID].newestLong.Equal(pollAt) {
		t.Fatalf("polled newestLong = %s, want the stored long window %s", got[polled.ID].newestLong, pollAt)
	}
	if !got[shortOnly.ID].newestLong.IsZero() {
		t.Fatalf("short-only newestLong = %s, want zero: short windows do not count", got[shortOnly.ID].newestLong)
	}
	if !got[disabled.ID].disabled || got[apiKey.ID].oauth || !got[busy.ID].oauth {
		t.Fatalf("flags: disabled=%+v apiKey=%+v busy=%+v", got[disabled.ID], got[apiKey.ID], got[busy.ID])
	}
	if poll := polls[shortOnly.ID]; poll.url != "http://upstream.test"+usageTestCodexUsagePath || poll.target.endpoint != usageEndpointCodexUsage {
		t.Fatalf("codex poll = %+v, want the Codex usage URL", poll)
	}
	if poll := polls[busy.ID]; poll.url != "http://upstream.test"+usageTestClaudeUsagePath || poll.target.endpoint != usageEndpointClaudeUsage {
		t.Fatalf("claude poll = %+v, want the Claude usage URL", poll)
	}
}

// newObserverHarness is a usage cache harness whose routing enables the poller,
// with a second Claude credential and credentials the poller must skip.
func newObserverHarness(t *testing.T, routing config.RoutingConfig) (*usageCacheHarness, *coreauth.Auth) {
	t.Helper()
	hs := newUsageCacheHarness(t, routing)
	claudeB := hs.register(t, usageTestClaudeAuth("claude-b.json", "claude-b-token"))
	disabled := usageTestClaudeAuth("claude-disabled.json", "claude-disabled-token")
	disabled.Disabled = true
	hs.register(t, disabled)
	hs.register(t, &coreauth.Auth{ID: "claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key-token"}})
	hs.register(t, &coreauth.Auth{ID: "xai.json", Provider: "xai", Metadata: map[string]any{"type": "xai", "access_token": "xai-token"}})
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, usageTestClaudeBody)
	hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBody)
	return hs, claudeB
}

// tick runs one poller tick and checks the upstream call totals afterwards.
func (hs *usageCacheHarness) tick(t *testing.T, step string, claudeCalls, codexCalls int) {
	t.Helper()
	hs.handler.observeQuotaOnce(context.Background())
	claude, codex := hs.upstream.callCount(usageTestClaudeUsagePath), hs.upstream.callCount(usageTestCodexUsagePath)
	if claude != claudeCalls || codex != codexCalls {
		t.Fatalf("%s: upstream calls claude=%d codex=%d, want claude=%d codex=%d", step, claude, codex, claudeCalls, codexCalls)
	}
}

// lastAuthorization returns the Authorization header of the newest call to path.
func (hs *usageCacheHarness) lastAuthorization(path string) string {
	headers := hs.upstream.requestHeaders(path)
	if len(headers) == 0 {
		return ""
	}
	return headers[len(headers)-1].Get("Authorization")
}

func TestQuotaObserverPollsIdleCredentials(t *testing.T) {
	t.Parallel()
	hs, claudeB := newObserverHarness(t, config.RoutingConfig{Strategy: "expiring-first"})

	hs.tick(t, "t0", 1, 1)
	claudeHeaders := hs.upstream.requestHeaders(usageTestClaudeUsagePath)[0]
	if claudeHeaders.Get("Authorization") != "Bearer claude-a-token" || claudeHeaders.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Fatalf("claude poll headers = %v", claudeHeaders)
	}
	codexHeaders := hs.upstream.requestHeaders(usageTestCodexUsagePath)[0]
	for name, want := range map[string]string{
		"Authorization":      "Bearer codex-c-token",
		"Content-Type":       "application/json",
		"OpenAI-Beta":        "codex-1",
		"Originator":         "Codex Desktop",
		"Chatgpt-Account-Id": "acc-codex-c",
	} {
		if got := codexHeaders.Get(name); got != want {
			t.Fatalf("codex poll header %s = %q, want %q", name, got, want)
		}
	}
	t0 := usageTestStart
	assertUsageReadings(t, hs.readings, hs.claude.ID, quotareading.SourcePoll, t0, map[string]float64{"5h": 12.5, "7d": 40})
	assertUsageReadings(t, hs.readings, hs.codex.ID, quotareading.SourcePoll, t0, map[string]float64{"primary": 25, "secondary": 60})

	// The poll warmed the cache for the panel and the T3 hub.
	expectUpstream(t, "warm cache", hs, usageTestClaudeUsagePath, hs.get(t, hs.claude, usageTestClaudeUsagePath), http.StatusOK, usageTestClaudeBody, usageCacheHit, 1)

	hs.clock.Advance(time.Minute)
	hs.tick(t, "t0+1m: claude min gap, fresh codex reading", 1, 1)

	hs.clock.Advance(9 * time.Minute)
	hs.tick(t, "t0+10m: the other claude credential", 2, 2)
	if got := hs.lastAuthorization(usageTestClaudeUsagePath); got != "Bearer claude-b-token" {
		t.Fatalf("t0+10m polled %q, want claude-b", got)
	}
	assertUsageReadings(t, hs.readings, claudeB.ID, quotareading.SourcePoll, t0.Add(10*time.Minute), map[string]float64{"5h": 12.5, "7d": 40})

	hs.clock.Advance(10 * time.Minute)
	hs.tick(t, "t0+20m: claude readings fresh", 2, 3)

	hs.clock.Advance(10 * time.Minute)
	hs.tick(t, "t0+30m: claude-a's reading reached the interval", 3, 4)
	if got := hs.lastAuthorization(usageTestClaudeUsagePath); got != "Bearer claude-a-token" {
		t.Fatalf("t0+30m polled %q, want claude-a", got)
	}
	for _, header := range hs.upstream.requestHeaders(usageTestClaudeUsagePath) {
		if auth := header.Get("Authorization"); auth == "Bearer claude-disabled-token" || auth == "Bearer claude-key-token" {
			t.Fatalf("polled a credential that must be skipped: %q", auth)
		}
	}
}

func TestQuotaObserverCountsEveryClaudeUsageCallForTheMinGap(t *testing.T) {
	t.Parallel()
	hs, _ := newObserverHarness(t, config.RoutingConfig{Strategy: "expiring-first"})
	// The T3 hub reads claude-a's usage: that is a Claude usage call too.
	expectUpstream(t, "hub", hs, usageTestClaudeUsagePath, hs.get(t, hs.claude, usageTestClaudeUsagePath), http.StatusOK, usageTestClaudeBody, usageCacheMiss, 1)

	hs.clock.Advance(10*time.Minute - time.Second)
	hs.tick(t, "inside the min gap", 1, 1)
	hs.clock.Advance(time.Second)
	hs.tick(t, "at the min gap", 2, 1)
	if got := hs.lastAuthorization(usageTestClaudeUsagePath); got != "Bearer claude-b-token" {
		t.Fatalf("polled %q, want claude-b: claude-a has a fresh reading from the hub", got)
	}
}

func TestQuotaObserverSkipsCredentialsInBackoff(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{
		Strategy: "expiring-first",
		QuotaObservation: config.QuotaObservationConfig{
			Poller: config.QuotaPollerConfig{ClaudeInterval: "1m", ClaudeMinGap: "1m"},
		},
	})
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusTooManyRequests, `{"error":"rate_limited"}`)
	hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBody)
	hs.tick(t, "t0: 429 starts a 5m backoff", 1, 1)

	// Without the backoff claude-a would be due: its interval and the min gap
	// are both 1m and it has no reading.
	hs.clock.Advance(5*time.Minute - time.Second)
	hs.tick(t, "inside backoff", 1, 1)
	hs.clock.Advance(time.Second)
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, usageTestClaudeBody)
	hs.tick(t, "after backoff", 2, 2)
	assertUsageReadings(t, hs.readings, hs.claude.ID, quotareading.SourcePoll, usageTestStart.Add(5*time.Minute), map[string]float64{"5h": 12.5, "7d": 40})
}

func TestQuotaObserverFollowsPollerConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		routing config.RoutingConfig
		polls   bool
	}{
		{name: "round-robin leaves it off", routing: config.RoutingConfig{Strategy: "round-robin"}},
		{name: "unset strategy leaves it off"},
		{name: "expiring-first turns it on", routing: config.RoutingConfig{Strategy: "expiring-first"}, polls: true},
		{name: "expiring-first alias turns it on", routing: config.RoutingConfig{Strategy: "soonest-reset"}, polls: true},
		{name: "explicit enable", polls: true, routing: config.RoutingConfig{Strategy: "round-robin", QuotaObservation: config.QuotaObservationConfig{
			Poller: config.QuotaPollerConfig{Enabled: new(true)},
		}}},
		{name: "explicit disable", routing: config.RoutingConfig{Strategy: "expiring-first", QuotaObservation: config.QuotaObservationConfig{
			Poller: config.QuotaPollerConfig{Enabled: new(false)},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs, _ := newObserverHarness(t, tc.routing)
			if tc.polls {
				hs.tick(t, "tick", 1, 1)
			} else {
				hs.tick(t, "tick", 0, 0)
			}
		})
	}
}

func TestQuotaObserverFollowsConfigReloads(t *testing.T) {
	t.Parallel()
	hs, _ := newObserverHarness(t, config.RoutingConfig{Strategy: "round-robin"})
	hs.tick(t, "round-robin", 0, 0)

	// A hot reload swaps the config pointer.
	hs.handler.SetConfig(&config.Config{Routing: config.RoutingConfig{Strategy: "expiring-first"}})
	hs.tick(t, "after reload to expiring-first", 1, 1)

	// A management write edits the config in place.
	hs.handler.mu.Lock()
	hs.handler.cfg.Routing.Strategy = "round-robin"
	hs.handler.mu.Unlock()
	hs.clock.Advance(time.Hour)
	hs.tick(t, "after switching back", 1, 1)
}

func TestQuotaObserverPollsWithCacheDisabled(t *testing.T) {
	t.Parallel()
	hs, _ := newObserverHarness(t, config.RoutingConfig{
		Strategy:         "expiring-first",
		QuotaObservation: config.QuotaObservationConfig{UsageCache: config.UsageCacheConfig{Enabled: new(false)}},
	})
	hs.tick(t, "t0", 1, 1)
	assertUsageReadings(t, hs.readings, hs.claude.ID, quotareading.SourcePoll, usageTestStart, map[string]float64{"5h": 12.5, "7d": 40})
	hs.clock.Advance(time.Minute)
	hs.tick(t, "t0+1m: min gap and per-credential interval still apply", 1, 1)
}

func TestQuotaPollHeaders(t *testing.T) {
	t.Parallel()
	withIDToken := usageTestCodexAuth("codex-c.json", "codex-token", "acc-from-id-token")
	withIDToken.Metadata["account_id"] = "acc-from-metadata"
	metadataOnly := &coreauth.Auth{ID: "codex-d.json", Provider: "codex", Metadata: map[string]any{"access_token": "t", "account_id": "acc-from-metadata"}}
	neither := &coreauth.Auth{ID: "codex-e.json", Provider: "codex", Metadata: map[string]any{"access_token": "t"}}
	for _, tc := range []struct {
		auth *coreauth.Auth
		want string
	}{{withIDToken, "acc-from-id-token"}, {metadataOnly, "acc-from-metadata"}, {neither, ""}, {nil, ""}} {
		if got := codexChatGPTAccountID(tc.auth); got != tc.want {
			t.Fatalf("codexChatGPTAccountID = %q, want %q", got, tc.want)
		}
	}

	codex := http.Header{}
	setQuotaPollHeaders(codex, neither, usageEndpointCodexUsage, "codex-token")
	if _, ok := codex["Chatgpt-Account-Id"]; ok || codex.Get("Authorization") != "Bearer codex-token" || codex.Get("Originator") != "Codex Desktop" {
		t.Fatalf("codex headers = %v, want no account header without an account", codex)
	}
	claude := http.Header{}
	setQuotaPollHeaders(claude, usageTestClaudeAuth("claude-a.json", "claude-token"), usageEndpointClaudeUsage, "claude-token")
	if len(claude) != 2 || claude.Get("Authorization") != "Bearer claude-token" || claude.Get("Anthropic-Beta") != "oauth-2025-04-20" {
		t.Fatalf("claude headers = %v, want Authorization and anthropic-beta only", claude)
	}
}

func TestStartQuotaObserverRunsOnce(t *testing.T) {
	t.Parallel()
	var nilHandler *Handler
	nilHandler.StartQuotaObserver(context.Background())

	h := &Handler{}
	t.Cleanup(func() { devyreStates.Delete(h) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.StartQuotaObserver(ctx)
	h.StartQuotaObserver(ctx)
	ran := false
	h.devyreState().observerOnce.Do(func() { ran = true })
	if ran {
		t.Fatal("StartQuotaObserver did not start the observer through its once")
	}
}

func TestQuotaObserverTickWithoutManagerOrConfig(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "expiring-first"}}}
	t.Cleanup(func() { devyreStates.Delete(h) })
	h.observeQuotaOnce(context.Background())

	empty := &Handler{}
	t.Cleanup(func() { devyreStates.Delete(empty) })
	empty.observeQuotaOnce(context.Background())
}
