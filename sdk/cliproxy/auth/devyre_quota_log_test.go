package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	log "github.com/sirupsen/logrus"
)

// logLines holds what the log file would hold: each entry as the router's own
// formatter writes it, which prints a message and a fixed list of fields.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Levels() []log.Level { return log.AllLevels }

func (l *logLines) Fire(entry *log.Entry) error {
	// A copy: the formatter writes into the entry's buffer when it has one.
	line, err := (&internallogging.LogFormatter{}).Format(&log.Entry{
		Logger: entry.Logger, Data: entry.Data, Time: entry.Time, Level: entry.Level, Message: entry.Message,
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.lines = append(l.lines, strings.TrimRight(string(line), "\n"))
	l.mu.Unlock()
	return nil
}

// of returns the lines that hold every part, in the order they were written.
func (l *logLines) of(parts ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []string
	for _, line := range l.lines {
		holds := true
		for _, part := range parts {
			holds = holds && strings.Contains(line, part)
		}
		if holds {
			matched = append(matched, line)
		}
	}
	return matched
}

// captureLog collects the standard logger's lines for the test.
func captureLog(t *testing.T) *logLines {
	t.Helper()
	lines := &logLines{}
	logger := log.StandardLogger()
	previous := logger.ReplaceHooks(make(log.LevelHooks))
	level := logger.GetLevel()
	logger.SetLevel(log.InfoLevel)
	logger.AddHook(lines)
	t.Cleanup(func() {
		logger.ReplaceHooks(previous)
		logger.SetLevel(level)
	})
	return lines
}

// oneLine returns the single line that holds every part.
func oneLine(t *testing.T, lines *logLines, parts ...string) string {
	t.Helper()
	matched := lines.of(parts...)
	if len(matched) != 1 {
		t.Fatalf("lines with %q = %q, want one; the log holds %q", parts, matched, lines.of())
	}
	return matched[0]
}

func expectParts(t *testing.T, line string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(line, part) {
			t.Fatalf("the line lacks %q: %s", part, line)
		}
	}
}

// The log names the limit a Claude account was rejected on. Before, the
// rejection read "parsed Anthropic rate limit reset headers", its windows and
// deadline in fields the formatter does not print, and nothing said what was
// held back, or until when.
func TestQuotaLogSaysWhichWindowsHeldAClaudeAccountBack(t *testing.T) {
	lines := captureLog(t)
	m, id := recoveryManager(t)
	rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
	expectParts(t, oneLine(t, lines, "quota: held", id),
		"provider=claude", "model=claude-opus-5-5", "scope=credential", "rejected=7d ", "until=", "in=24h0m0s")

	t.Run("both shared windows, with the claim Claude names", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day"},
		})
		wait := 3 * time.Hour
		m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-5-5", CredentialScope: true,
			RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests}})
		expectParts(t, oneLine(t, lines, "quota: held", id),
			"model=claude-sonnet-5-5", "scope=credential", "rejected=5h,7d claim=seven_day until=", "in=3h0m0s")
	})

	t.Run("a model's own weekly limit holds the model alone", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		rejectSubscription(m, id, "claude-fable-5-1", false)
		expectParts(t, oneLine(t, lines, "quota: held", id), "model=claude-fable-5-1", "scope=model", "rejected=7d:fable ")
	})

	t.Run("an ordinary rate limit names no window", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		wait := time.Minute
		m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5",
			RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "requests per minute"}})
		expectParts(t, oneLine(t, lines, "quota: held", id), "scope=model", "rejected=none until=")
	})

	t.Run("what Claude sends never breaks the line", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day until=1970 auth=someone"},
		})
		wait := time.Hour
		m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", CredentialScope: true,
			RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests}})
		line := oneLine(t, lines, "quota: held", id)
		if strings.Contains(line, "claim=") || strings.Contains(line, "someone") {
			t.Fatalf("a claim that is no plain name reached the log: %s", line)
		}
	})
}

func TestQuotaLogSaysWhatHeldAChatGPTAccountBack(t *testing.T) {
	lines := captureLog(t)
	m, id := codexRecoveryManager(t)
	rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 5*time.Hour)
	expectParts(t, oneLine(t, lines, "quota: held", id),
		"provider=codex", "model=gpt-6.1-sol", "scope=credential", "rejected=usage_limit until=", "in=5h0m0s")

	t.Run("the window its headers report full", func(t *testing.T) {
		lines := captureLog(t)
		m, id := codexRecoveryManager(t)
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"X-Codex-Primary-Used-Percent":          {"37"},
			"X-Codex-Primary-Window-Minutes":        {"300"},
			"X-Codex-Primary-Reset-After-Seconds":   {"3600"},
			"X-Codex-Secondary-Used-Percent":        {"100"},
			"X-Codex-Secondary-Window-Minutes":      {"10080"},
			"X-Codex-Secondary-Reset-After-Seconds": {"86400"},
		})
		wait := 24 * time.Hour
		m.MarkResult(ctx, Result{AuthID: id, Provider: "codex", Model: "gpt-6.1-sol", CredentialScope: true,
			RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: codexUsageLimitBody}})
		expectParts(t, oneLine(t, lines, "quota: held", id), "rejected=secondary until=")
	})
}

// Nothing is said of what held nothing back.
func TestQuotaLogSaysNothingWithoutAHold(t *testing.T) {
	lines := captureLog(t)
	m, id := recoveryManager(t)
	ctx := context.Background()
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", Success: true})
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", Error: &Error{HTTPStatus: http.StatusBadGateway}})
	m.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5", Error: &Error{HTTPStatus: http.StatusUnauthorized}})
	if held := lines.of("quota:", id); len(held) != 0 {
		t.Fatalf("lines for results that hold nothing back: %q", held)
	}
}

// The log says when a hold is lifted, and by what.
func TestQuotaLogSaysWhenAHoldIsLifted(t *testing.T) {
	lines := captureLog(t)
	m, id := recoveryManager(t)
	rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
	reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
	expectParts(t, oneLine(t, lines, "quota: back in use", id), "provider=claude", "by=usage")
	if kept := lines.of("quota: hold kept", id); len(kept) != 0 {
		t.Fatalf("a lifted hold is said to be kept: %q", kept)
	}

	codex, codexID := codexRecoveryManager(t)
	rejectCodexUsageLimit(codex, codexID, "gpt-6.1-sol", 24*time.Hour)
	reconcileCodexUsage(codex, codexID, time.Now().Add(time.Second))
	expectParts(t, oneLine(t, lines, "quota: back in use", codexID), "provider=codex", "by=usage")

	// A cooldown reset a client asked for says so too.
	auth, _ := m.GetByID(id)
	DevyreLogQuotaLifted(auth, "reset")
	expectParts(t, oneLine(t, lines, "quota: back in use", id, "by=reset"), "provider=claude")
}

// And why a fresh usage reading left a hold standing: without it, a hold that
// was read and kept looks like one nobody read.
func TestQuotaLogSaysWhyAFreshUsageKeptAHold(t *testing.T) {
	spent := []quotareading.Window{
		{ID: "5h", Kind: quotareading.KindShort, UsedPercent: 20},
		{ID: "7d", Kind: quotareading.KindLong, UsedPercent: 100},
	}
	t.Run("the usage still shows the limit", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
		reconcileUsage(m, id, spent, time.Now().Add(time.Second))
		expectParts(t, oneLine(t, lines, "quota: hold kept", id), "provider=claude", `why="its usage does not show room at every limit"`)
	})
	t.Run("the usage contradicts nothing on record", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		wait := time.Hour
		m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-opus-5-5",
			RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "requests per minute"}})
		reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
		expectParts(t, oneLine(t, lines, "quota: hold kept", id), `why="its usage contradicts no rejection on record"`)
	})
	t.Run("the credential changed while its usage was read", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
		observed, _ := m.GetByID(id)
		rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
		m.ReconcileClaudeUsage(observed, recoveredUsage(), nil, time.Now().Add(time.Second))
		expectParts(t, oneLine(t, lines, "quota: hold kept", id), `why="it changed while its usage was read"`)
	})
	t.Run("a ChatGPT account's usage without room", func(t *testing.T) {
		lines := captureLog(t)
		m, id := codexRecoveryManager(t)
		rejectCodexUsageLimit(m, id, "gpt-6.1-sol", 24*time.Hour)
		observed, _ := m.GetByID(id)
		m.ReconcileCodexUsage(observed, false, time.Now().Add(time.Second))
		expectParts(t, oneLine(t, lines, "quota: hold kept", id), "provider=codex", `why="its usage does not show room at every limit"`)
	})
	t.Run("nothing of a credential no limit holds back", func(t *testing.T) {
		lines := captureLog(t)
		m, id := recoveryManager(t)
		reconcileUsage(m, id, spent, time.Now().Add(time.Second))
		reconcileUsage(m, id, recoveredUsage(), time.Now().Add(time.Second))
		if said := lines.of("quota:", id); len(said) != 0 {
			t.Fatalf("lines for a credential nothing holds back: %q", said)
		}
	})
}
