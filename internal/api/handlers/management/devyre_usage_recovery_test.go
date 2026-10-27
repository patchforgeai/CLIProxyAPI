package management

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeUsageRefreshRecoversRejectedSubscription(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	blockUsageAccount(t, hs)
	// The usage call starts after the failed request and reports a restored plan.
	hs.clock.mu.Lock()
	hs.clock.now = time.Now().Add(time.Second)
	hs.clock.mu.Unlock()
	hs.upstream.respondJSON(usageTestClaudeUsagePath, 200, restoredClaudeUsage)
	result := hs.mustDo(t, usageTestCall{
		route: "/v8/management/requests/api-call", authIndex: hs.claude.Index,
		method: http.MethodGet, url: hs.url(usageTestClaudeUsagePath), refresh: true,
	})
	if result.code != 200 || result.resp.StatusCode != 200 {
		t.Fatalf("usage refresh failed: %+v", result)
	}
	assertUsageBlocked(t, hs, false)
}

const restoredClaudeUsage = `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":0,"resets_at":null}}`

func blockUsageAccount(t *testing.T, hs *usageCacheHarness) {
	t.Helper()
	ctx := logging.WithResponseHeadersHolder(context.Background())
	reset := time.Now().Add(24 * time.Hour)
	logging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Reset":  {strconv.FormatInt(reset.Unix(), 10)},
	})
	delay := 24 * time.Hour
	hs.manager.MarkResult(ctx, coreauth.Result{
		AuthID: hs.claude.ID, Provider: "claude", Model: "claude-opus-5-5",
		CredentialScope: true, RetryAfter: &delay,
		Error: &coreauth.Error{HTTPStatus: 429, Message: "subscription window rejected"},
	})
	blocked, _ := hs.manager.GetByID(hs.claude.ID)
	if !blocked.Unavailable || !blocked.Quota.Exceeded {
		t.Fatal("expected an active subscription cooldown before recovery")
	}
}

func assertUsageBlocked(t *testing.T, hs *usageCacheHarness, want bool) {
	t.Helper()
	auth, _ := hs.manager.GetByID(hs.claude.ID)
	blocked := auth.Unavailable || auth.Quota.Exceeded || auth.ModelStates["claude-opus-5-5"].Unavailable
	if blocked != want {
		t.Fatalf("blocked=%v, want %v: quota=%+v model=%+v", blocked, want, auth.Quota, auth.ModelStates["claude-opus-5-5"])
	}
}

func TestClaudeUsageRefreshDoesNotRecoverFromCachedOrPartialReadings(t *testing.T) {
	for _, scenario := range []string{"cache hit", "stale fallback", "variant", "profile", "malformed", "missing window"} {
		t.Run(scenario, func(t *testing.T) {
			hs := newUsageCacheHarness(t, config.RoutingConfig{})
			hs.upstream.respondJSON(usageTestClaudeUsagePath, 200, restoredClaudeUsage)
			hs.get(t, hs.claude, usageTestClaudeUsagePath)
			blockUsageAccount(t, hs)
			path := usageTestClaudeUsagePath
			switch scenario {
			case "cache hit":
				hs.get(t, hs.claude, path)
				assertUsageBlocked(t, hs, true)
				return
			case "stale fallback":
				hs.upstream.respondJSON(path, 503, `{"error":"unavailable"}`)
			case "variant":
				path += "?cedar_ember=1"
			case "profile":
				path = usageTestClaudeProfilePath
				hs.upstream.respondJSON(path, 200, restoredClaudeUsage)
			case "malformed":
				hs.upstream.respondJSON(path, 200, `{"five_hour":`)
			case "missing window":
				hs.upstream.respondJSON(path, 200, `{"five_hour":{"utilization":0}}`)
			}
			hs.clock.mu.Lock()
			hs.clock.now = time.Now().Add(time.Second)
			hs.clock.mu.Unlock()
			hs.refresh(t, hs.claude, path)
			assertUsageBlocked(t, hs, true)
		})
	}
}

func TestClaudeUsagePollRecoversRejectedSubscription(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	blockUsageAccount(t, hs)
	hs.clock.mu.Lock()
	hs.clock.now = time.Now().Add(time.Second)
	hs.clock.mu.Unlock()
	hs.upstream.respondJSON(usageTestClaudeUsagePath, 200, restoredClaudeUsage)
	auth, _ := hs.manager.GetByID(hs.claude.ID)
	url := hs.url(usageTestClaudeUsagePath)
	hs.handler.pollUsage(context.Background(), hs.cache, quotaPoll{auth: auth, url: url, target: hs.cache.targets[url]}, config.UsageCacheConfig{})
	assertUsageBlocked(t, hs, false)
}
