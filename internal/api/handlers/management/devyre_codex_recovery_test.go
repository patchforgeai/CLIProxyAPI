package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A ChatGPT account whose usage limit rejected a request is held back until
// the reset that rejection named. A reset of its limits (a reset credit
// redeemed in the ChatGPT app or in Codex) lifts the limit at once, which only
// its usage says. Before, no usage response lifted the hold: the account
// stayed out of use until the old deadline or the router's next start.

const (
	codexRecoveryModel = "gpt-6.1-sol"
	// ChatGPT's refusal of a request for the account's usage limit.
	codexUsageLimitRejection = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":86400}}`
)

// blockCodexAccount fails a request of the harness's ChatGPT account on its
// usage limit, as the Codex executor reports one: credential-wide, until the
// reset it named.
func blockCodexAccount(t *testing.T, hs *usageCacheHarness) {
	t.Helper()
	delay := 24 * time.Hour
	hs.manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: hs.codex.ID, Provider: "codex", Model: codexRecoveryModel,
		CredentialScope: true, RetryAfter: &delay,
		Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: codexUsageLimitRejection},
	})
	if !codexAccountBlocked(hs) {
		t.Fatal("expected an active usage-limit cooldown before recovery")
	}
	// The usage call starts after the failed request.
	hs.clock.mu.Lock()
	hs.clock.now = time.Now().Add(time.Second)
	hs.clock.mu.Unlock()
}

func codexAccountBlocked(hs *usageCacheHarness) bool {
	auth, _ := hs.manager.GetByID(hs.codex.ID)
	state := auth.ModelStates[codexRecoveryModel]
	return auth.Unavailable || auth.Quota.Exceeded || (state != nil && state.Unavailable)
}

func TestCodexUsageRefreshRecoversAnAccountAtItsUsageLimit(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	blockCodexAccount(t, hs)
	hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBodyWith(0))
	result := hs.refresh(t, hs.codex, usageTestCodexUsagePath)
	if result.code != http.StatusOK || result.resp.StatusCode != http.StatusOK {
		t.Fatalf("usage refresh failed: %+v", result)
	}
	if codexAccountBlocked(hs) {
		auth, _ := hs.manager.GetByID(hs.codex.ID)
		t.Fatalf("the account is still held back after its usage read as started over: quota=%+v", auth.Quota)
	}
}

// The idle poller reads a ChatGPT account's usage every few minutes: a reset
// used outside the router is found by it without anyone asking.
func TestCodexUsagePollRecoversAnAccountAtItsUsageLimit(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	blockCodexAccount(t, hs)
	hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBodyWith(0))
	auth, _ := hs.manager.GetByID(hs.codex.ID)
	url := hs.url(usageTestCodexUsagePath)
	hs.handler.pollUsage(context.Background(), hs.cache, quotaPoll{auth: auth, url: url, target: hs.cache.targets[url]}, config.UsageCacheConfig{})
	if codexAccountBlocked(hs) {
		t.Fatal("the poller's fresh usage did not lift the hold")
	}
}

func TestCodexUsageRefreshKeepsAHoldItDoesNotContradict(t *testing.T) {
	const room = `{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":600}`
	const spent = `{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":600}`
	scenarios := map[string]string{
		"a window at its limit":       `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":` + spent + `}}`,
		"limit reached":               `{"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":` + room + `}}`,
		"not allowed":                 `{"rate_limit":{"allowed":false,"limit_reached":false,"primary_window":` + room + `}}`,
		"no window listed":            `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":null,"secondary_window":null}}`,
		"a window without its usage":  `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":600}}}`,
		"a model's own limit reached": `{"rate_limit":{"primary_window":` + room + `},"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":` + spent + `}}]}`,
		"malformed":                   `{"rate_limit":`,
	}
	for name, body := range scenarios {
		t.Run(name, func(t *testing.T) {
			hs := newUsageCacheHarness(t, config.RoutingConfig{})
			blockCodexAccount(t, hs)
			hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, body)
			hs.refresh(t, hs.codex, usageTestCodexUsagePath)
			if !codexAccountBlocked(hs) {
				t.Fatal("a usage response that shows no room lifted the hold")
			}
		})
	}
	t.Run("a cached response", func(t *testing.T) {
		hs := newUsageCacheHarness(t, config.RoutingConfig{})
		hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBodyWith(0))
		hs.get(t, hs.codex, usageTestCodexUsagePath)
		blockCodexAccount(t, hs)
		// Read before the rejection and answered from the cache: it says nothing of it.
		hs.clock.mu.Lock()
		hs.clock.now = usageTestStart.Add(time.Second)
		hs.clock.mu.Unlock()
		if result := hs.get(t, hs.codex, usageTestCodexUsagePath); result.label() != usageCacheHit {
			t.Fatalf("expected a cache hit, got %q", result.label())
		}
		if !codexAccountBlocked(hs) {
			t.Fatal("a cached usage response lifted the hold")
		}
	})
	t.Run("a per-minute rate limit", func(t *testing.T) {
		hs := newUsageCacheHarness(t, config.RoutingConfig{})
		delay := 10 * time.Minute
		hs.manager.MarkResult(context.Background(), coreauth.Result{
			AuthID: hs.codex.ID, Provider: "codex", Model: codexRecoveryModel, RetryAfter: &delay,
			Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"type":"rate_limit_exceeded","message":"Rate limit reached"}}`},
		})
		hs.clock.mu.Lock()
		hs.clock.now = time.Now().Add(time.Second)
		hs.clock.mu.Unlock()
		hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBodyWith(0))
		hs.refresh(t, hs.codex, usageTestCodexUsagePath)
		auth, _ := hs.manager.GetByID(hs.codex.ID)
		if state := auth.ModelStates[codexRecoveryModel]; state == nil || !state.Unavailable {
			t.Fatal("usage lifted a rate limit it says nothing of")
		}
	})
}

// With several accounts, the reset is one account's: the other stays held.
func TestCodexUsageRecoveryIsTheAccountsOwn(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	other := hs.register(t, usageTestCodexAuth("codex-d.json", "codex-d-token", "acc-codex-d"))
	delay := 24 * time.Hour
	hs.manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: other.ID, Provider: "codex", Model: codexRecoveryModel,
		CredentialScope: true, RetryAfter: &delay,
		Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: codexUsageLimitRejection},
	})
	blockCodexAccount(t, hs)
	hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBodyWith(0))
	hs.refresh(t, hs.codex, usageTestCodexUsagePath)
	if codexAccountBlocked(hs) {
		t.Fatal("the account whose usage was read is still held back")
	}
	if held, _ := hs.manager.GetByID(other.ID); !held.Unavailable || !held.Quota.Exceeded {
		t.Fatal("another account's usage lifted this account's hold")
	}
}
