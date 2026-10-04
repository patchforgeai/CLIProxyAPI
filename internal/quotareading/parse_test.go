package quotareading

import (
	"testing"
	"time"
)

func TestQuotaReadingFromSignalsDispatch(t *testing.T) {
	claudeSignals := map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5"}
	codexSignals := map[string]string{"X-Codex-Primary-Used-Percent": "50"}
	tests := []struct {
		provider string
		signals  map[string]string
		wantIDs  []string
	}{
		{provider: "claude", signals: claudeSignals, wantIDs: []string{"7d"}},
		{provider: " Claude ", signals: claudeSignals, wantIDs: []string{"7d"}},
		{provider: "codex", signals: codexSignals, wantIDs: []string{"primary"}},
		{provider: "CODEX", signals: codexSignals, wantIDs: []string{"primary"}},
		{provider: "claude", signals: codexSignals, wantIDs: nil},
		{provider: "codex", signals: claudeSignals, wantIDs: nil},
		{provider: "gemini", signals: claudeSignals, wantIDs: nil},
		{provider: "", signals: codexSignals, wantIDs: nil},
	}
	for _, tc := range tests {
		got := FromSignals(tc.provider, tc.signals, testNow)
		if len(got) != len(tc.wantIDs) {
			t.Fatalf("FromSignals(%q) = %v, want IDs %v", tc.provider, formatWindows(got), tc.wantIDs)
		}
		for i, id := range tc.wantIDs {
			if got[i].ID != id || got[i].Source != SourceHeader || !got[i].ObservedAt.Equal(testNow) {
				t.Fatalf("FromSignals(%q)[%d] = %s, want ID %s", tc.provider, i, formatWindow(got[i]), id)
			}
		}
	}
}

func TestQuotaReadingFromUsageBodyDispatch(t *testing.T) {
	claudeBody := []byte(`{"seven_day": {"utilization": 35.0, "resets_at": "2026-10-08T10:00:00+00:00"}}`)
	codexBody := []byte(codexUsageFixture)
	profileBody := []byte(`{"account": {"has_claude_max": true}, "organization": {"organization_type": "claude_max"}}`)
	tests := []struct {
		name     string
		provider string
		url      string
		body     []byte
		wantIDs  []string
		wantErr  bool
	}{
		{name: "claude usage", provider: "claude", url: ClaudeUsageURL, body: claudeBody, wantIDs: []string{"7d"}},
		{name: "codex usage", provider: "codex", url: CodexUsageURL, body: codexBody, wantIDs: []string{"primary"}},
		{name: "provider inferred from the URL", provider: "", url: CodexUsageURL, body: codexBody, wantIDs: []string{"primary"}},
		{name: "trailing slash and query", provider: "Claude", url: " https://API.anthropic.com/api/oauth/usage/?beta=1 ", body: claudeBody, wantIDs: []string{"7d"}},
		{name: "claude profile is not a usage body", provider: "claude", url: ClaudeProfileURL, body: profileBody},
		{name: "codex credits are not a usage body", provider: "codex", url: "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", body: codexBody},
		{name: "provider must own the endpoint", provider: "claude", url: CodexUsageURL, body: codexBody},
		{name: "other host with the same path", provider: "claude", url: "https://example.com/api/oauth/usage", body: claudeBody},
		{name: "unparseable URL", provider: "claude", url: "://bad", body: claudeBody},
		{name: "invalid usage body", provider: "claude", url: ClaudeUsageURL, body: []byte(`<html>`), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromUsageBody(tc.provider, tc.url, tc.body, testNow)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %v, want IDs %v", formatWindows(got), tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id || got[i].Source != SourceUsage {
					t.Fatalf("window %d = %s, want ID %s from usage", i, formatWindow(got[i]), id)
				}
			}
		})
	}
}

func TestQuotaReadingUsageSnapshot(t *testing.T) {
	claudeBody := `{"five_hour": {"utilization": 12.5, "resets_at": "2026-10-03T23:00:00+00:00"}, "seven_day": {"utilization": 35.0, "resets_at": "2026-10-08T10:00:00+00:00"}}`
	tests := []struct {
		name     string
		provider string
		url      string
		body     string
		wantOK   bool
		wantIDs  []string
		wantErr  bool
	}{
		{name: "claude usage", provider: "claude", url: ClaudeUsageURL, body: claudeBody, wantOK: true, wantIDs: []string{"5h", "7d"}},
		{
			name: "claude usage with a reset-grant block", provider: "claude", url: ClaudeUsageURL + "?cedar_ember=1&skip_spend=1",
			body:   `{"five_hour": {"utilization": 99, "resets_at": "2026-10-03T23:00:00+00:00"}, "seven_day": null, "cedar_ember": {"eligible": true, "grants": []}}`,
			wantOK: true, wantIDs: []string{"5h"},
		},
		{name: "claude usage reporting no windows", provider: "claude", url: ClaudeUsageURL, body: `{"five_hour": null, "seven_day": null}`, wantOK: true},
		{name: "a reset-grant status alone is not a usage body", provider: "claude", url: ClaudeUsageURL + "?cedar_ember=1&skip_spend=1", body: `{"cedar_ember": {"eligible": true, "grants": []}}`},
		{name: "scoped limits alone are not a usage body", provider: "claude", url: ClaudeUsageURL, body: `{"limits": [{"kind": "weekly_scoped", "percent": 50, "scope": {"model": {"display_name": "Fable 5"}}}]}`},
		{name: "a claude window that is not an object", provider: "claude", url: ClaudeUsageURL, body: `{"five_hour": 12, "seven_day": "35"}`},
		{name: "codex usage", provider: "codex", url: CodexUsageURL, body: codexUsageFixture, wantOK: true, wantIDs: []string{"primary"}},
		{name: "codex usage reporting no rate limit", provider: "codex", url: CodexUsageURL, body: `{"plan_type": "free", "rate_limit": null}`, wantOK: true},
		{name: "codex camelCase rate limit", provider: "", url: CodexUsageURL, body: `{"rateLimit": {"primaryWindow": {"usedPercent": 12, "limitWindowSeconds": 18000}}}`, wantOK: true, wantIDs: []string{"primary"}},
		{name: "a codex body without a rate limit", provider: "codex", url: CodexUsageURL, body: `{"plan_type": "pro"}`},
		{name: "claude profile is not a usage endpoint", provider: "claude", url: ClaudeProfileURL, body: claudeBody},
		{name: "provider must own the endpoint", provider: "codex", url: ClaudeUsageURL, body: claudeBody},
		{name: "invalid usage body", provider: "claude", url: ClaudeUsageURL, body: `<html>`, wantErr: true},
		{name: "a JSON array is not a usage body", provider: "codex", url: CodexUsageURL, body: `[1]`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := UsageSnapshot(tc.provider, tc.url, []byte(tc.body), testNow)
			if (err != nil) != tc.wantErr || ok != tc.wantOK {
				t.Fatalf("UsageSnapshot ok = %v, error = %v; want ok %v, wantErr %v", ok, err, tc.wantOK, tc.wantErr)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %v, want IDs %v", formatWindows(got), tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id || got[i].Source != SourceUsage || !got[i].ObservedAt.Equal(testNow) {
					t.Fatalf("window %d = %s, want ID %s observed now from usage", i, formatWindow(got[i]), id)
				}
			}
		})
	}
}

func TestQuotaReadingParseInstant(t *testing.T) {
	want := time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)
	tests := []struct {
		raw  string
		want time.Time
	}{
		{raw: "1791061200", want: want},
		{raw: "1791061200.5", want: want.Add(500 * time.Millisecond)},
		{raw: "1791061200000", want: want},
		{raw: "2026-10-03T21:00:00Z", want: want},
		{raw: "2026-10-03T14:00:00-07:00", want: want},
		{raw: "2026-10-03T21:00:00.000000000123+00:00", want: want},
		{raw: "Sat, 03 Oct 2026 21:00:00 GMT", want: want},
		{raw: "", want: time.Time{}},
		{raw: "0", want: time.Time{}},
		{raw: "-5", want: time.Time{}},
		{raw: "NaN", want: time.Time{}},
		{raw: "tomorrow", want: time.Time{}},
	}
	for _, tc := range tests {
		if got := parseInstant(tc.raw); !got.Equal(tc.want) {
			t.Fatalf("parseInstant(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestQuotaReadingSignalLookupIsDeterministic(t *testing.T) {
	signals := map[string]string{
		"x-codex-primary-used-percent": "10",
		"X-Codex-Primary-Used-Percent": "20",
		"X-CODEX-PRIMARY-USED-PERCENT": "30",
	}
	for i := 0; i < 50; i++ {
		got := FromCodexHeaders(signals, testNow)
		if len(got) != 1 || got[0].UsedPercent != 10 {
			t.Fatalf("iteration %d: got %v, want the lexicographically greatest spelling (used 10)", i, formatWindows(got))
		}
	}
}
