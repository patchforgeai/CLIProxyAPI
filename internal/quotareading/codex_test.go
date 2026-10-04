package quotareading

import (
	"math"
	"testing"
	"time"
)

func TestQuotaReadingFromCodexHeaders(t *testing.T) {
	observed := testNow
	week := 7 * 24 * time.Hour
	header := func(id string, kind Kind, used float64, resets time.Time, length time.Duration) Window {
		return Window{ID: id, Kind: kind, UsedPercent: used, ResetsAt: resets, Length: length, ObservedAt: observed, Source: SourceHeader}
	}

	tests := []struct {
		name         string
		signals      map[string]string
		zeroObserved bool
		want         []Window
	}{
		{
			name:    "nil signals",
			signals: nil,
			want:    nil,
		},
		{
			// Websocket quota event fixture (helps/codex_quota_test.go): a weekly
			// primary window. reset-at wins over reset-after-seconds.
			name: "weekly primary prefers reset-at",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "48",
				"X-Codex-Primary-Window-Minutes":      "10080",
				"X-Codex-Primary-Reset-After-Seconds": "523210",
				"X-Codex-Primary-Reset-At":            "1786677299",
				"X-Codex-Plan-Type":                   "pro",
				"X-Codex-Credits-Balance":             "0",
			},
			want: []Window{header("primary", KindLong, 48, time.Unix(1786677299, 0), week)},
		},
		{
			// Error frame fixture: only reset-after-seconds, relative to observedAt.
			name: "exhausted window with relative reset",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "100",
				"X-Codex-Primary-Window-Minutes":      "10080",
				"X-Codex-Primary-Reset-After-Seconds": "437380",
			},
			want: []Window{header("primary", KindLong, 100, observed.Add(437380*time.Second), week)},
		},
		{
			name: "five-hour primary and weekly secondary",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":                 "42.5",
				"X-Codex-Primary-Window-Minutes":               "300",
				"X-Codex-Primary-Reset-After-Seconds":          "600",
				"X-Codex-Secondary-Used-Percent":               "17",
				"X-Codex-Secondary-Window-Minutes":             "10080",
				"X-Codex-Secondary-Reset-At":                   "1787290791",
				"X-Codex-Primary-Over-Secondary-Limit-Percent": "0",
			},
			want: []Window{
				header("primary", KindShort, 42.5, observed.Add(10*time.Minute), 5*time.Hour),
				header("secondary", KindLong, 17, time.Unix(1787290791, 0), week),
			},
		},
		{
			name: "unknown lengths fall back to primary short and secondary long",
			signals: map[string]string{
				"x-codex-primary-used-percent":   "5",
				"x-codex-secondary-used-percent": "60",
				"x-codex-secondary-reset-at":     "1787290791",
			},
			want: []Window{
				header("primary", KindShort, 5, time.Time{}, 0),
				header("secondary", KindLong, 60, time.Unix(1787290791, 0), 0),
			},
		},
		{
			name: "a 24 hour window still only gates",
			signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   "10",
				"X-Codex-Secondary-Window-Minutes": "1440",
			},
			want: []Window{header("secondary", KindShort, 10, time.Time{}, 24*time.Hour)},
		},
		{
			name: "limit reached without used-percent counts as exhausted",
			signals: map[string]string{
				"X-Codex-Limit-Reached":            "true",
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         "1787231961",
				"X-Codex-Secondary-Window-Minutes": "10080",
			},
			want: []Window{header("primary", KindShort, 100, time.Unix(1787231961, 0), 5*time.Hour)},
		},
		{
			name: "additional and code-review limits are ignored",
			signals: map[string]string{
				"X-Codex-Bengalfox-Primary-Used-Percent":                      "90",
				"X-Codex-Additional-Gpt-5.3-Codex-Spark-Primary-Used-Percent": "80",
				"X-Codex-Code-Review-Primary-Used-Percent":                    "70",
			},
			want: nil,
		},
		{
			name: "out of range percentages",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "-1",
				"X-Codex-Secondary-Used-Percent": "101",
			},
			want: []Window{header("secondary", KindLong, 100, time.Time{}, 0)},
		},
		{
			name: "an absurd window length saturates instead of overflowing",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "20",
				"X-Codex-Primary-Window-Minutes": "1e300",
			},
			want: []Window{header("primary", KindLong, 20, time.Time{}, time.Duration(math.MaxInt64))},
		},
		{
			name:         "relative reset is unknown without an observation time",
			zeroObserved: true,
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "30",
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Reset-After-Seconds": "60",
			},
			want: []Window{{ID: "primary", Kind: KindShort, UsedPercent: 30, Length: 5 * time.Hour, Source: SourceHeader}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := observed
			if tc.zeroObserved {
				at = time.Time{}
			}
			assertWindows(t, FromCodexHeaders(tc.signals, at), tc.want)
		})
	}
}

// codexUsageFixture is the current /wham/usage payload from the panel tests
// (tests/codexQuota.test.ts): a weekly primary window and no secondary window.
const codexUsageFixture = `{
	"plan_type": "pro",
	"rate_limit": {
		"allowed": true,
		"limit_reached": false,
		"primary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_after_seconds": 601888, "reset_at": 1785902974},
		"secondary_window": null
	},
	"code_review_rate_limit": null,
	"additional_rate_limits": [{
		"limit_name": "GPT-5.3-Codex-Spark",
		"metered_feature": "codex_bengalfox",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 602111, "reset_at": 1785903197},
			"secondary_window": null
		}
	}],
	"rate_limit_reset_credits": {"available_count": 1, "applicable_available_count": 0}
}`

func TestQuotaReadingFromCodexUsage(t *testing.T) {
	observed := testNow
	week := 7 * 24 * time.Hour
	usage := func(id string, kind Kind, used float64, resets time.Time, length time.Duration) Window {
		return Window{ID: id, Kind: kind, UsedPercent: used, ResetsAt: resets, Length: length, ObservedAt: observed, Source: SourceUsage}
	}

	tests := []struct {
		name string
		body string
		want []Window
	}{
		{
			// reset_at is unix seconds: the panel renders it with
			// formatUnixSeconds (value * 1000). 1785902974 is 2026-08-05T04:09:34Z.
			name: "panel fixture pins reset_at as unix seconds",
			body: codexUsageFixture,
			want: []Window{usage("primary", KindLong, 1, time.Date(2026, 8, 5, 4, 9, 34, 0, time.UTC), week)},
		},
		{
			name: "five-hour primary and weekly secondary",
			body: `{"rate_limit": {
				"primary_window": {"used_percent": 73.5, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": 1785000000},
				"secondary_window": {"used_percent": "22", "limit_window_seconds": "604800", "reset_at": "1785500000"}
			}}`,
			want: []Window{
				usage("primary", KindShort, 73.5, time.Unix(1785000000, 0), 5*time.Hour),
				usage("secondary", KindLong, 22, time.Unix(1785500000, 0), week),
			},
		},
		{
			name: "camelCase spellings",
			body: `{"rateLimit": {
				"limitReached": false,
				"primaryWindow": {"usedPercent": 12, "limitWindowSeconds": 18000, "resetAfterSeconds": 900},
				"secondaryWindow": {"usedPercent": 34, "limitWindowSeconds": 2592000, "resetAt": 1787000000}
			}}`,
			want: []Window{
				usage("primary", KindShort, 12, observed.Add(15*time.Minute), 5*time.Hour),
				usage("secondary", KindLong, 34, time.Unix(1787000000, 0), 30*24*time.Hour),
			},
		},
		{
			name: "millisecond reset_at is recognized by magnitude",
			body: `{"rate_limit": {"primary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_at": 1785902974000}}}`,
			want: []Window{usage("primary", KindLong, 1, time.Unix(1785902974, 0), week)},
		},
		{
			name: "limit reached without used_percent counts as exhausted",
			body: `{"rate_limit": {"limit_reached": true, "primary_window": {"limit_window_seconds": 18000, "reset_after_seconds": 120}, "secondary_window": {"limit_window_seconds": 604800}}}`,
			want: []Window{usage("primary", KindShort, 100, observed.Add(2*time.Minute), 5*time.Hour)},
		},
		{
			name: "unknown window lengths fall back to primary short and secondary long",
			body: `{"rate_limit": {"primary_window": {"used_percent": 3}, "secondary_window": {"used_percent": 9, "reset_at": 0, "reset_after_seconds": 30}}}`,
			want: []Window{
				usage("primary", KindShort, 3, time.Time{}, 0),
				usage("secondary", KindLong, 9, observed.Add(30*time.Second), 0),
			},
		},
		{
			name: "missing rate limit",
			body: `{"plan_type": "free", "rate_limit": null}`,
			want: nil,
		},
		{
			name: "non-finite numbers are rejected",
			body: `{"rate_limit": {"primary_window": {"used_percent": 1e400}, "secondary_window": {"used_percent": 7, "limit_window_seconds": -1e400}}}`,
			want: []Window{usage("secondary", KindLong, 7, time.Time{}, 0)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromCodexUsage([]byte(tc.body), observed)
			if err != nil {
				t.Fatalf("FromCodexUsage error: %v", err)
			}
			assertWindows(t, got, tc.want)
		})
	}
}

func TestQuotaReadingFromCodexUsageRejectsInvalidBodies(t *testing.T) {
	for _, body := range []string{``, `{`, `[1,2]`, `42`} {
		if got, err := FromCodexUsage([]byte(body), testNow); err == nil {
			t.Fatalf("body %q: expected an error, got windows %v", body, formatWindows(got))
		}
	}
}
