package quotareading

import (
	"math"
	"testing"
	"time"
)

const defaultGate = 2.0

func longWindow(remaining float64, resetsIn time.Duration) Window {
	return Window{ID: "7d", Kind: KindLong, UsedPercent: 100 - remaining, ResetsAt: testNow.Add(resetsIn), Length: 7 * 24 * time.Hour, ObservedAt: testNow, Source: SourceUsage}
}

func shortWindow(remaining float64, resetsIn time.Duration) Window {
	return Window{ID: "5h", Kind: KindShort, UsedPercent: 100 - remaining, ResetsAt: testNow.Add(resetsIn), Length: 5 * time.Hour, ObservedAt: testNow, Source: SourceHeader}
}

func scopedWindow(family string, remaining float64, resetsIn time.Duration) Window {
	return Window{ID: "7d:" + family, Kind: KindScoped, Model: family, UsedPercent: 100 - remaining, ResetsAt: testNow.Add(resetsIn), Length: 7 * 24 * time.Hour, ObservedAt: testNow, Source: SourceUsage}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func withID(w Window, id string) Window {
	w.ID = id
	return w
}

func TestQuotaReadingEvaluateUrgencyOrdering(t *testing.T) {
	tests := []struct {
		name               string
		first, second      Window
		wantFirst, wantSec float64
	}{
		{
			// A: 40% left resetting in 3h must burn before B: 90% left for 4 days.
			name:      "A beats B",
			first:     longWindow(40, 3*time.Hour),
			second:    longWindow(90, 4*24*time.Hour),
			wantFirst: 40.0 / 3, wantSec: 90.0 / 96,
		},
		{
			// D: 80% left in 2h beats C: 1% left in 10 min (floored to 15 min).
			name:      "D beats C",
			first:     longWindow(80, 2*time.Hour),
			second:    longWindow(1, 10*time.Minute),
			wantFirst: 40, wantSec: 4,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := Evaluate(Reading{AuthID: "first", Windows: []Window{tc.first}}, "claude-opus-5-5", testNow, defaultGate)
			second := Evaluate(Reading{AuthID: "second", Windows: []Window{tc.second}}, "claude-opus-5-5", testNow, defaultGate)
			if !first.UrgencyKnown || !second.UrgencyKnown || !first.Usable || !second.Usable {
				t.Fatalf("evaluations = %+v / %+v", first, second)
			}
			if !approxEqual(first.Urgency, tc.wantFirst) || !approxEqual(second.Urgency, tc.wantSec) {
				t.Fatalf("urgencies = %v / %v, want %v / %v", first.Urgency, second.Urgency, tc.wantFirst, tc.wantSec)
			}
			if first.Urgency <= second.Urgency {
				t.Fatalf("first urgency %v must exceed second %v", first.Urgency, second.Urgency)
			}
			if first.RankWindowID != "7d" || second.RankWindowID != "7d" {
				t.Fatalf("rank windows = %q / %q", first.RankWindowID, second.RankWindowID)
			}
		})
	}
}

func TestQuotaReadingEvaluateGates(t *testing.T) {
	week := longWindow(60, 3*24*time.Hour)
	tests := []struct {
		name        string
		windows     []Window
		model       string
		gate        float64
		wantUsable  bool
		wantReason  string
		wantResetIn time.Duration
	}{
		{
			name:        "exhausted 5h gates",
			windows:     []Window{shortWindow(0, 90*time.Minute), week},
			model:       "claude-opus-5-5",
			gate:        defaultGate,
			wantReason:  "5h exhausted",
			wantResetIn: 90 * time.Minute,
		},
		{
			name:        "remaining exactly at the gate gates",
			windows:     []Window{shortWindow(2, time.Hour), week},
			model:       "claude-sonnet-5",
			gate:        defaultGate,
			wantReason:  "5h exhausted",
			wantResetIn: time.Hour,
		},
		{
			name:       "remaining above the gate is usable",
			windows:    []Window{shortWindow(2.5, time.Hour), week},
			model:      "claude-sonnet-5",
			gate:       defaultGate,
			wantUsable: true,
		},
		{
			name:       "a zero gate only gates a fully used window",
			windows:    []Window{shortWindow(1, time.Hour), week},
			gate:       0,
			wantUsable: true,
		},
		{
			name:       "an exhausted window whose reset passed does not gate",
			windows:    []Window{shortWindow(0, -time.Minute), week},
			gate:       defaultGate,
			wantUsable: true,
		},
		{
			name:       "an exhausted window with an unknown reset does not gate",
			windows:    []Window{{ID: "5h", Kind: KindShort, UsedPercent: 100, Length: 5 * time.Hour}, week},
			gate:       defaultGate,
			wantUsable: true,
		},
		{
			name:       "an exhausted long window ranks but never gates",
			windows:    []Window{longWindow(0, 24*time.Hour)},
			gate:       defaultGate,
			wantUsable: true,
		},
		{
			name:        "Fable-scoped window gates Fable models",
			windows:     []Window{scopedWindow("fable", 0, 2*24*time.Hour), week},
			model:       "claude-fable-5",
			gate:        defaultGate,
			wantReason:  "7d:fable exhausted",
			wantResetIn: 2 * 24 * time.Hour,
		},
		{
			name:       "Fable-scoped window does not gate Opus",
			windows:    []Window{scopedWindow("fable", 0, 2*24*time.Hour), week},
			model:      "claude-opus-5-5",
			gate:       defaultGate,
			wantUsable: true,
		},
		{
			name:       "Fable-scoped window does not gate a model-agnostic view",
			windows:    []Window{scopedWindow("fable", 0, 2*24*time.Hour), week},
			model:      "",
			gate:       defaultGate,
			wantUsable: true,
		},
		{
			name:        "several gates report every window and the latest reset",
			windows:     []Window{scopedWindow("opus", 1, 3*24*time.Hour), shortWindow(0, 2*time.Hour), week},
			model:       "Claude-Opus-5-5",
			gate:        defaultGate,
			wantReason:  "5h exhausted, 7d:opus exhausted",
			wantResetIn: 3 * 24 * time.Hour,
		},
		{
			name:       "a NaN gate never gates",
			windows:    []Window{shortWindow(0, time.Hour)},
			gate:       math.NaN(),
			wantUsable: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(Reading{AuthID: "auth-a", Provider: "claude", Windows: tc.windows}, tc.model, testNow, tc.gate)
			if got.Usable != tc.wantUsable || got.GateReason != tc.wantReason {
				t.Fatalf("Evaluate = %+v, want usable %v reason %q", got, tc.wantUsable, tc.wantReason)
			}
			wantReset := time.Time{}
			if !tc.wantUsable {
				wantReset = testNow.Add(tc.wantResetIn)
			}
			if !got.GateResetsAt.Equal(wantReset) {
				t.Fatalf("GateResetsAt = %v, want %v", got.GateResetsAt, wantReset)
			}
		})
	}
}

func TestQuotaReadingEvaluatePassedResetCountsAsFullWindow(t *testing.T) {
	store := NewStore()
	stale := longWindow(5, -time.Hour)
	stale.ObservedAt = testNow.Add(-8 * 24 * time.Hour)
	store.Put("fresh-week", "claude", []Window{stale})
	store.Put("burning", "claude", []Window{longWindow(40, 3*time.Hour)})

	fresh := Evaluate(Effective(store, "fresh-week", "claude", nil, time.Time{}, testNow), "claude-opus-5-5", testNow, defaultGate)
	if !fresh.Usable || !fresh.UrgencyKnown || fresh.RankWindowID != "7d" {
		t.Fatalf("passed reset evaluation = %+v", fresh)
	}
	wantFresh := 100 / (7*24 - 1.0)
	if !approxEqual(fresh.Urgency, wantFresh) {
		t.Fatalf("passed reset urgency = %v, want a full window at %v", fresh.Urgency, wantFresh)
	}
	burning := Evaluate(Effective(store, "burning", "claude", nil, time.Time{}, testNow), "claude-opus-5-5", testNow, defaultGate)
	if burning.Urgency <= fresh.Urgency {
		t.Fatalf("a freshly reset window (%v) must rank below one about to expire (%v)", fresh.Urgency, burning.Urgency)
	}
}

func TestQuotaReadingEvaluateNoData(t *testing.T) {
	for _, reading := range []Reading{
		{AuthID: "empty"},
		{AuthID: "unknown-reset", Windows: []Window{{ID: "7d", Kind: KindLong, UsedPercent: 0, Length: 7 * 24 * time.Hour}}},
		{AuthID: "stale-reset", Windows: []Window{longWindow(50, -time.Minute)}},
		{AuthID: "other-family-only", Windows: []Window{scopedWindow("fable", 50, time.Hour)}},
	} {
		got := Evaluate(reading, "claude-opus-5-5", testNow, defaultGate)
		if got.UrgencyKnown || got.Urgency != 0 || !got.Usable {
			t.Fatalf("%s: Evaluate = %+v, want usable with unknown urgency", reading.AuthID, got)
		}
	}
	if got := Evaluate(Reading{}, "", testNow, defaultGate); got.RankWindowID != "" {
		t.Fatalf("empty reading has rank window %q", got.RankWindowID)
	}
}

func TestQuotaReadingEvaluateRankingWindow(t *testing.T) {
	month := Window{ID: "secondary", Kind: KindLong, UsedPercent: 50, ResetsAt: testNow.Add(10 * 24 * time.Hour), Length: 30 * 24 * time.Hour}
	weekEarly := Window{ID: "b-week", Kind: KindLong, UsedPercent: 50, ResetsAt: testNow.Add(24 * time.Hour), Length: 7 * 24 * time.Hour}
	weekLate := Window{ID: "a-week", Kind: KindLong, UsedPercent: 50, ResetsAt: testNow.Add(48 * time.Hour), Length: 7 * 24 * time.Hour}
	weekUnknown := Window{ID: "0-week", Kind: KindLong, UsedPercent: 50, Length: 7 * 24 * time.Hour}
	tests := []struct {
		name    string
		windows []Window
		model   string
		wantID  string
		urgency float64
	}{
		{name: "longest long window", windows: []Window{weekEarly, month, shortWindow(50, time.Hour)}, wantID: "secondary", urgency: 50.0 / 240},
		{name: "tie prefers the earliest reset", windows: []Window{weekLate, weekEarly}, wantID: "b-week", urgency: 50.0 / 24},
		{name: "tie prefers a known reset", windows: []Window{weekUnknown, weekLate}, wantID: "a-week", urgency: 50.0 / 48},
		{name: "full tie prefers the smaller ID", windows: []Window{weekEarly, withID(weekEarly, "a-twin")}, wantID: "a-twin", urgency: 50.0 / 24},
		{name: "long beats a longer scoped window", windows: []Window{scopedWindow("opus", 10, 6*24*time.Hour), longWindow(80, 24*time.Hour)}, model: "opus", wantID: "7d", urgency: 80.0 / 24},
		{name: "same-family scoped window ranks without a long window", windows: []Window{shortWindow(90, time.Hour), scopedWindow("fable", 30, 12*time.Hour)}, model: "claude-fable-5", wantID: "7d:fable", urgency: 30.0 / 12},
		{name: "other-family scoped window is skipped", windows: []Window{shortWindow(90, 4*time.Hour), scopedWindow("fable", 30, 12*time.Hour)}, model: "claude-opus-5-5", wantID: "5h", urgency: 90.0 / 4},
		{name: "urgency floors the hours at a quarter", windows: []Window{longWindow(10, time.Minute)}, wantID: "7d", urgency: 40},
		{
			name:    "codex weekly-only primary ranks",
			windows: []Window{{ID: "primary", Kind: KindLong, UsedPercent: 1, ResetsAt: testNow.Add(50 * time.Hour), Length: 7 * 24 * time.Hour}},
			wantID:  "primary",
			urgency: 99.0 / 50,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(Reading{AuthID: "auth-a", Windows: tc.windows}, tc.model, testNow, defaultGate)
			if got.RankWindowID != tc.wantID || !got.UrgencyKnown || !approxEqual(got.Urgency, tc.urgency) {
				t.Fatalf("Evaluate = %+v, want rank %q urgency %v", got, tc.wantID, tc.urgency)
			}
		})
	}
}

func TestQuotaReadingKindAndModelFamily(t *testing.T) {
	for kind, want := range map[Kind]string{KindShort: "short", KindLong: "long", KindScoped: "scoped", Kind(42): "unknown"} {
		if got := kind.String(); got != want {
			t.Fatalf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
	for model, want := range map[string]string{
		"claude-fable-5":             "fable",
		"Fable 5":                    "fable",
		"claude-opus-5-5":            "opus",
		"claude-opus-4-1-20250805":   "opus",
		"CLAUDE-SONNET-5":            "sonnet",
		"claude-haiku-4-5":           "haiku",
		"gpt-5.3-codex":              "",
		"":                           "",
		"claude-3-7-sonnet-thinking": "sonnet",
	} {
		if got := ModelFamily(model); got != want {
			t.Fatalf("ModelFamily(%q) = %q, want %q", model, got, want)
		}
	}
	if got := (Window{UsedPercent: 30}).RemainingPercent(); got != 70 {
		t.Fatalf("RemainingPercent = %v, want 70", got)
	}
	if got := (Window{UsedPercent: 130}).RemainingPercent(); got != 0 {
		t.Fatalf("RemainingPercent above 100 used = %v, want 0", got)
	}
	if got := (Window{UsedPercent: -30}).RemainingPercent(); got != 100 {
		t.Fatalf("RemainingPercent below 0 used = %v, want 100", got)
	}
	if got := (Window{UsedPercent: math.NaN()}).RemainingPercent(); got != 0 {
		t.Fatalf("RemainingPercent of NaN = %v, want 0", got)
	}
}
