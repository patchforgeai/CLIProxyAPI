package quotareading

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQuotaReadingFromClaudeHeaders(t *testing.T) {
	observed := testNow.Add(-time.Minute)
	reset5h := testNow.Add(5 * time.Hour).Truncate(time.Second)
	reset7d := testNow.Add(6 * 24 * time.Hour).Truncate(time.Second)
	unix := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	short := func(used float64, resets time.Time) Window {
		return Window{ID: "5h", Kind: KindShort, UsedPercent: used, ResetsAt: resets, Length: 5 * time.Hour, ObservedAt: observed, Source: SourceHeader}
	}
	week := func(used float64, resets time.Time) Window {
		return Window{ID: "7d", Kind: KindLong, UsedPercent: used, ResetsAt: resets, Length: 7 * 24 * time.Hour, ObservedAt: observed, Source: SourceHeader}
	}
	fable := func(used float64, resets time.Time) Window {
		return Window{ID: "7d:fable", Kind: KindScoped, UsedPercent: used, ResetsAt: resets, Length: 7 * 24 * time.Hour, Model: "fable", ObservedAt: observed, Source: SourceHeader}
	}

	tests := []struct {
		name    string
		signals map[string]string
		want    []Window
	}{
		{
			name:    "nil signals",
			signals: nil,
			want:    nil,
		},
		{
			// Exact headers from upstream issue #5915 (claude_ratelimit_test.go):
			// utilization is a fraction and 1.02 clamps to 100.
			name: "issue 5915 overage rejection",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":                  "rejected",
				"Anthropic-Ratelimit-Unified-Representative-Claim":    "seven_day_overage_included",
				"Anthropic-Ratelimit-Unified-7d-Status":               "allowed",
				"Anthropic-Ratelimit-Unified-7d-Utilization":          "0.69",
				"Anthropic-Ratelimit-Unified-5h-Utilization":          "0.00",
				"Anthropic-Ratelimit-Unified-7d_oi-Status":            "rejected",
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization":       "1.02",
				"Anthropic-Ratelimit-Unified-Overage-Status":          "rejected",
				"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": "org_spend_cap_reached",
				"Retry-After": "121180",
			},
			want: []Window{short(0, time.Time{}), week(69, time.Time{}), fable(100, time.Time{})},
		},
		{
			// "5h rejected and 7d allowed with unified reset": a rejected status
			// without utilization means the window is exhausted.
			name: "rejected status without utilization",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
				"Anthropic-Ratelimit-Unified-5h-Reset":  unix(reset5h),
				"Anthropic-Ratelimit-Unified-7d-Status": "allowed",
				"Anthropic-Ratelimit-Unified-7d-Reset":  unix(reset7d),
				"Anthropic-Ratelimit-Unified-Reset":     unix(reset5h),
			},
			want: []Window{short(100, reset5h)},
		},
		{
			name: "fractions with unix resets",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Status":         "allowed",
				"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.25",
				"Anthropic-Ratelimit-Unified-5h-Reset":          unix(reset5h),
				"Anthropic-Ratelimit-Unified-7d-Status":         "allowed_warning",
				"Anthropic-Ratelimit-Unified-7d-Utilization":    "0.875",
				"Anthropic-Ratelimit-Unified-7d-Reset":          unix(reset7d),
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.5",
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":       unix(reset7d),
			},
			want: []Window{short(25, reset5h), week(87.5, reset7d), fable(50, reset7d)},
		},
		{
			name: "keys match case-insensitively and resets accept RFC 3339",
			signals: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": " 0.1 ",
				"ANTHROPIC-RATELIMIT-UNIFIED-5H-RESET":       reset5h.Format(time.RFC3339),
			},
			want: []Window{short(10, reset5h)},
		},
		{
			name: "rejected status overrides a lower utilization",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Status":      "Rejected",
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.97",
				"Anthropic-Ratelimit-Unified-7d-Reset":       unix(reset7d),
			},
			want: []Window{week(100, reset7d)},
		},
		{
			name: "invalid utilizations are skipped",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization":    "NaN",
				"Anthropic-Ratelimit-Unified-7d-Utilization":    "-0.1",
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "invalid",
				"Anthropic-Ratelimit-Unified-7d_oi-Status":      "allowed",
			},
			want: nil,
		},
		{
			name: "infinite utilization is skipped and an unparseable reset is unknown",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "+Inf",
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
				"Anthropic-Ratelimit-Unified-7d-Reset":       "soon",
			},
			want: []Window{week(40, time.Time{})},
		},
		{
			name: "unrelated signals are ignored",
			signals: map[string]string{
				"Retry-After":                  "60",
				"X-Codex-Primary-Used-Percent": "50",
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertWindows(t, FromClaudeHeaders(tc.signals, observed), tc.want)
		})
	}
}

func TestQuotaReadingFromClaudeUsage(t *testing.T) {
	observed := testNow
	modernReset := "2026-10-07T10:00:00.000000+00:00"
	legacyReset := "2026-10-08T10:00:00.000000+00:00"
	fiveHourReset := "2026-10-03T23:59:59.943648+00:00"
	week := 7 * 24 * time.Hour
	usage := func(id string, kind Kind, model string, used float64, resets time.Time, length time.Duration) Window {
		return Window{ID: id, Kind: kind, UsedPercent: used, ResetsAt: resets, Length: length, Model: model, ObservedAt: observed, Source: SourceUsage}
	}

	tests := []struct {
		name string
		body string
		want []Window
	}{
		{
			// Shape of GET /api/oauth/usage as the panel parser reads it
			// (src/features/quota/providers/claude/data.ts and its tests).
			name: "full payload with modern Fable limit",
			body: `{
				"five_hour": {"utilization": 6.0, "resets_at": "` + fiveHourReset + `"},
				"seven_day": {"utilization": 35.0, "resets_at": "` + legacyReset + `"},
				"seven_day_oauth_apps": null,
				"seven_day_opus": {"utilization": 12.0, "resets_at": null},
				"seven_day_sonnet": {"utilization": 3.5, "resets_at": "` + legacyReset + `"},
				"seven_day_cowork": {"utilization": 50.0, "resets_at": "` + legacyReset + `"},
				"iguana_necktie": {"utilization": 41, "resets_at": "` + legacyReset + `"},
				"extra_usage": {"is_enabled": false, "monthly_limit": 0, "used_credits": 0, "utilization": null},
				"limits": [
					{"kind": "weekly_scoped", "group": "weekly", "percent": 64, "resets_at": "` + modernReset + `", "is_active": true,
					 "scope": {"model": {"id": null, "display_name": "Fable"}}}
				]
			}`,
			want: []Window{
				usage("5h", KindShort, "", 6, mustTime(t, fiveHourReset), 5*time.Hour),
				usage("7d", KindLong, "", 35, mustTime(t, legacyReset), week),
				usage("7d:fable", KindScoped, "fable", 64, mustTime(t, modernReset), week),
				usage("7d:opus", KindScoped, "opus", 12, time.Time{}, week),
				usage("7d:sonnet", KindScoped, "sonnet", 3.5, mustTime(t, legacyReset), week),
			},
		},
		{
			name: "legacy Fable key without limits",
			body: `{"iguana_necktie": {"utilization": 41, "resets_at": "` + legacyReset + `"}}`,
			want: []Window{usage("7d:fable", KindScoped, "fable", 41, mustTime(t, legacyReset), week)},
		},
		{
			name: "legacy Fable key when the modern percent is invalid",
			body: `{
				"iguana_necktie": {"utilization": 41, "resets_at": "` + legacyReset + `"},
				"limits": [{"kind": "weekly_scoped", "percent": null, "resets_at": "` + modernReset + `", "is_active": true,
				            "scope": {"model": {"display_name": "Fable"}}}]
			}`,
			want: []Window{usage("7d:fable", KindScoped, "fable", 41, mustTime(t, legacyReset), week)},
		},
		{
			name: "active modern entry wins over an earlier inactive one",
			body: `{
				"iguana_necktie": {"utilization": 41, "resets_at": "` + legacyReset + `"},
				"limits": [
					{"kind": "weekly_scoped", "percent": 12, "resets_at": "` + legacyReset + `", "is_active": false,
					 "scope": {"model": {"display_name": "Fable 5"}}},
					{"kind": "weekly_scoped", "percent": 64, "resets_at": "` + modernReset + `", "is_active": true,
					 "scope": {"model": {"display_name": "Fable"}}}
				]
			}`,
			want: []Window{usage("7d:fable", KindScoped, "fable", 64, mustTime(t, modernReset), week)},
		},
		{
			name: "first valid entry wins when none is active",
			body: `{"limits": [
				{"kind": "weekly_scoped", "percent": null, "is_active": true, "scope": {"model": {"display_name": "Fable"}}},
				{"kind": "weekly_scoped", "percent": 30, "resets_at": "` + modernReset + `", "is_active": false, "scope": {"model": {"display_name": "Fable"}}},
				{"kind": "weekly_scoped", "percent": 90, "resets_at": "` + legacyReset + `", "is_active": false, "scope": {"model": {"display_name": "Fable"}}}
			]}`,
			want: []Window{usage("7d:fable", KindScoped, "fable", 30, mustTime(t, modernReset), week)},
		},
		{
			name: "limits entry replaces the named window of the same family",
			body: `{
				"seven_day_sonnet": {"utilization": 3, "resets_at": "` + legacyReset + `"},
				"limits": [{"kind": "WEEKLY_SCOPED", "percent": "35.5", "resets_at": "` + modernReset + `",
				            "scope": {"model": {"display_name": "Claude Sonnet 5"}}}]
			}`,
			want: []Window{usage("7d:sonnet", KindScoped, "sonnet", 35.5, mustTime(t, modernReset), week)},
		},
		{
			name: "malformed and unrelated limits are ignored",
			body: `{
				"five_hour": {"utilization": 10, "resets_at": null},
				"seven_day": {"utilization": 20, "resets_at": "` + legacyReset + `"},
				"limits": [
					null,
					"weekly_scoped",
					{"kind": "session", "percent": 50, "scope": {"model": {"display_name": "Fable"}}},
					{"kind": "weekly_scoped", "percent": 70, "scope": {"model": {"display_name": "Claude Design"}}},
					{"kind": "weekly_scoped", "percent": -5, "scope": {"model": {"display_name": "Opus"}}}
				]
			}`,
			want: []Window{
				usage("5h", KindShort, "", 10, time.Time{}, 5*time.Hour),
				usage("7d", KindLong, "", 20, mustTime(t, legacyReset), week),
			},
		},
		{
			name: "utilization above 100 clamps and missing utilization is skipped",
			body: `{"five_hour": {"utilization": 104.2, "resets_at": "` + fiveHourReset + `"}, "seven_day": {"resets_at": "` + legacyReset + `"}}`,
			want: []Window{usage("5h", KindShort, "", 100, mustTime(t, fiveHourReset), 5*time.Hour)},
		},
		{
			name: "empty object",
			body: `{}`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromClaudeUsage([]byte(tc.body), observed)
			if err != nil {
				t.Fatalf("FromClaudeUsage error: %v", err)
			}
			assertWindows(t, got, tc.want)
		})
	}
}

func TestQuotaReadingFromClaudeUsageRejectsInvalidBodies(t *testing.T) {
	for _, body := range []string{``, `not json`, `{"five_hour":`, `[]`, `null`, `"text"`} {
		got, err := FromClaudeUsage([]byte(body), testNow)
		if err == nil {
			t.Fatalf("body %q: expected an error, got windows %v", body, formatWindows(got))
		}
		if strings.Contains(err.Error(), body) && body != "" {
			t.Fatalf("body %q leaked into the error: %v", body, err)
		}
	}
}
