package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const exhaustedClaudeUsage = `{"five_hour":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":100,"resets_at":"2099-01-01T00:00:00Z"}}`

// resetCooldown posts the cooldown reset of auth, as a client does once the
// provider granted the account a reset.
func resetCooldown(t *testing.T, hs *usageCacheHarness, auth *coreauth.Auth) {
	t.Helper()
	hs.router.POST("/v8/management/routing/cooldown/reset", hs.handler.ResetQuota)
	req := httptest.NewRequest(http.MethodPost, "/v8/management/routing/cooldown/reset", strings.NewReader(`{"auth_index":"`+auth.Index+`"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	hs.router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cooldown reset answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

// effectiveReading is what the selector reads of auth: the stored windows
// merged with the header signals its last response left on the credential.
func effectiveReading(hs *usageCacheHarness, auth *coreauth.Auth) quotareading.Reading {
	current, _ := hs.manager.GetByID(auth.ID)
	return quotareading.Effective(hs.readings, current.ID, current.Provider, current.Quota.Signals, current.Quota.ObservedAt, hs.clock.Now())
}

// A cooldown reset says the credential's quota started over. The cooldown was
// cleared before, but the readings still said the credential was exhausted: its
// last usage body, and the rejection's own headers, which read "rejected" until
// the next response. The selector kept gating the credential on them, and the
// readings endpoint kept listing it at its limit, until its usage was read
// again, which Claude may refuse for minutes after a reset.
func TestCooldownResetForgetsWhatWasReadOfTheQuotaBefore(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	path := usageTestClaudeUsagePath
	hs.upstream.respondJSON(path, http.StatusOK, exhaustedClaudeUsage)
	hs.get(t, hs.claude, path)
	blockUsageAccount(t, hs)
	hs.clock.mu.Lock()
	hs.clock.now = time.Now().Add(time.Second)
	hs.clock.mu.Unlock()
	gate := config.ExpiringFirstConfig{}.GatePercent()
	if before := effectiveReading(hs, hs.claude); quotareading.Evaluate(before, "claude-opus-5-5", hs.clock.Now(), gate).Usable {
		t.Fatalf("precondition: the readings do not gate the credential: %+v", before.Windows)
	}

	resetCooldown(t, hs, hs.claude)

	assertUsageBlocked(t, hs, false)
	after := effectiveReading(hs, hs.claude)
	if len(after.Windows) != 0 {
		t.Fatalf("readings from before the reset still stand: %+v", after.Windows)
	}
	if !quotareading.Evaluate(after, "claude-opus-5-5", hs.clock.Now(), gate).Usable {
		t.Fatal("the credential is still gated after its cooldown was reset")
	}

	// Its cached usage is stale too: the next read asks the provider, inside the
	// TTL and without a refresh, and what it answers is recorded.
	calls := hs.upstream.callCount(path)
	hs.clock.mu.Lock()
	hs.clock.now = hs.clock.now.Add(time.Second)
	hs.clock.mu.Unlock()
	hs.upstream.respondJSON(path, http.StatusOK, restoredClaudeUsage)
	if result := hs.get(t, hs.claude, path); result.label() != usageCacheMiss || hs.upstream.callCount(path) != calls+1 {
		t.Fatalf("usage after the reset: label %q, %d upstream calls, want a miss and %d", result.label(), hs.upstream.callCount(path), calls+1)
	}
	fresh := effectiveReading(hs, hs.claude)
	if len(fresh.Windows) != 2 || fresh.Windows[0].UsedPercent != 0 || fresh.Windows[1].UsedPercent != 0 {
		t.Fatalf("readings after the reset = %+v, want both windows empty", fresh.Windows)
	}
}

// The reset is one credential's: another's readings and cached usage stand.
func TestCooldownResetLeavesOtherCredentialsAlone(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	other := hs.register(t, usageTestClaudeAuth("claude-b.json", "claude-b-token"))
	path := usageTestClaudeUsagePath
	hs.upstream.respondJSON(path, http.StatusOK, exhaustedClaudeUsage)
	hs.get(t, hs.claude, path)
	hs.get(t, other, path)
	calls := hs.upstream.callCount(path)

	resetCooldown(t, hs, hs.claude)

	if kept := hs.readings.Get(other.ID); len(kept.Windows) != 2 {
		t.Fatalf("the other credential's readings = %+v, want both windows kept", kept.Windows)
	}
	if result := hs.get(t, other, path); result.label() != usageCacheHit || hs.upstream.callCount(path) != calls {
		t.Fatalf("the other credential's usage: label %q, %d upstream calls, want a hit and %d", result.label(), hs.upstream.callCount(path), calls)
	}
}

// logMessages collects the messages the standard logger writes during a test.
type logMessages struct {
	mu       sync.Mutex
	messages []string
}

func (l *logMessages) Levels() []log.Level { return log.AllLevels }

func (l *logMessages) Fire(entry *log.Entry) error {
	l.mu.Lock()
	l.messages = append(l.messages, entry.Message)
	l.mu.Unlock()
	return nil
}

// with returns the messages that hold every part.
func (l *logMessages) with(parts ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []string
	for _, message := range l.messages {
		holds := true
		for _, part := range parts {
			holds = holds && strings.Contains(message, part)
		}
		if holds {
			matched = append(matched, message)
		}
	}
	return matched
}

// The log says that a cooldown reset put the credential back in use: a hold
// that ends before its time is otherwise not told from one nobody touched.
func TestCooldownResetIsSaidInTheLog(t *testing.T) {
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	blockUsageAccount(t, hs)
	said := &logMessages{}
	logger := log.StandardLogger()
	previous := logger.ReplaceHooks(make(log.LevelHooks))
	logger.AddHook(said)
	t.Cleanup(func() { logger.ReplaceHooks(previous) })

	resetCooldown(t, hs, hs.claude)

	if lines := said.with("quota: back in use", "provider=claude", "by=reset"); len(lines) != 1 {
		t.Fatalf("lines for the reset = %q, want one", lines)
	}
}
