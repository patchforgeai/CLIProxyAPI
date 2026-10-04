package quotareading

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestQuotaReadingStorePutKeepsNewestPerWindow(t *testing.T) {
	t0 := testNow.Add(-time.Hour)
	t1 := testNow
	week := Window{ID: "7d", Kind: KindLong, UsedPercent: 40, Length: 7 * 24 * time.Hour, ObservedAt: t1, Source: SourceUsage}
	short := Window{ID: "5h", Kind: KindShort, UsedPercent: 10, Length: 5 * time.Hour, ObservedAt: t0, Source: SourceHeader}

	tests := []struct {
		name string
		puts [][]Window
		want map[string]float64 // used percent per window ID
	}{
		{
			name: "older observation is ignored",
			puts: [][]Window{{week, short}, {withUsage(week, 99, t0)}},
			want: map[string]float64{"7d": 40, "5h": 10},
		},
		{
			name: "equal observation replaces",
			puts: [][]Window{{week, short}, {withUsage(week, 55, t1)}},
			want: map[string]float64{"7d": 55, "5h": 10},
		},
		{
			name: "newer observation replaces only its own window",
			puts: [][]Window{{week, short}, {withUsage(short, 70, t1)}},
			want: map[string]float64{"7d": 40, "5h": 70},
		},
		{
			name: "out of order puts converge on the newest",
			puts: [][]Window{{withUsage(week, 80, t1.Add(time.Minute))}, {week}, {withUsage(week, 5, t0)}},
			want: map[string]float64{"7d": 80},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			for _, put := range tc.puts {
				store.Put("auth-a", "claude", put)
			}
			got := store.Get("auth-a")
			if got.AuthID != "auth-a" || got.Provider != "claude" {
				t.Fatalf("reading identity = %q/%q", got.AuthID, got.Provider)
			}
			used := map[string]float64{}
			for _, w := range got.Windows {
				used[w.ID] = w.UsedPercent
			}
			if fmt.Sprint(used) != fmt.Sprint(tc.want) {
				t.Fatalf("used = %v, want %v", used, tc.want)
			}
		})
	}
}

func withUsage(w Window, used float64, observedAt time.Time) Window {
	w.UsedPercent = used
	w.ObservedAt = observedAt
	return w
}

func TestQuotaReadingStoreSanitizesInput(t *testing.T) {
	store := NewStore()
	store.Put("", "claude", []Window{{ID: "7d", UsedPercent: 10, ObservedAt: testNow}})
	store.Put("auth-a", " Codex ", []Window{
		{ID: "", UsedPercent: 10, ObservedAt: testNow},
		{ID: "primary", UsedPercent: math.NaN(), ObservedAt: testNow},
		{ID: "secondary", UsedPercent: 150, ObservedAt: testNow},
		{ID: "tertiary", UsedPercent: -3, ObservedAt: testNow},
	})
	store.Put("auth-b", "codex", nil)

	snapshot := store.Snapshot()
	if len(snapshot) != 1 || snapshot[0].AuthID != "auth-a" || snapshot[0].Provider != "codex" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	got := snapshot[0].Windows
	if len(got) != 2 || got[0].ID != "secondary" || got[0].UsedPercent != 100 || got[1].ID != "tertiary" || got[1].UsedPercent != 0 {
		t.Fatalf("sanitized windows = %v", formatWindows(got))
	}
	if reading := store.Get("missing"); reading.AuthID != "missing" || reading.Provider != "" || reading.Windows != nil {
		t.Fatalf("unknown credential reading = %+v", reading)
	}
}

func TestQuotaReadingStoreForgetAndSnapshotCopies(t *testing.T) {
	store := NewStore()
	store.Put("auth-c", "codex", []Window{{ID: "primary", UsedPercent: 30, ObservedAt: testNow}})
	store.Put("auth-a", "claude", []Window{{ID: "7d", UsedPercent: 10, ObservedAt: testNow}, {ID: "5h", UsedPercent: 20, ObservedAt: testNow}})
	store.Put("auth-b", "claude", []Window{{ID: "7d", UsedPercent: 50, ObservedAt: testNow}})

	snapshot := store.Snapshot()
	var order []string
	for _, reading := range snapshot {
		order = append(order, reading.AuthID)
	}
	if fmt.Sprint(order) != "[auth-a auth-b auth-c]" {
		t.Fatalf("snapshot order = %v", order)
	}
	if ids := []string{snapshot[0].Windows[0].ID, snapshot[0].Windows[1].ID}; fmt.Sprint(ids) != "[5h 7d]" {
		t.Fatalf("windows are not sorted by ID: %v", ids)
	}

	snapshot[0].Windows[0].UsedPercent = 99
	snapshot[0].Windows = append(snapshot[0].Windows, Window{ID: "extra"})
	got := store.Get("auth-a")
	got.Windows[1].UsedPercent = 77
	again := store.Get("auth-a")
	if len(again.Windows) != 2 || again.Windows[0].UsedPercent != 20 || again.Windows[1].UsedPercent != 10 {
		t.Fatalf("store was mutated through a returned copy: %v", formatWindows(again.Windows))
	}

	store.Forget("auth-a")
	store.Forget("never-stored")
	if reading := store.Get("auth-a"); len(reading.Windows) != 0 || reading.Provider != "" {
		t.Fatalf("forgotten credential still has readings: %+v", reading)
	}
	if len(store.Snapshot()) != 2 {
		t.Fatalf("forget removed more than one credential")
	}
}

// A Codex account moves from {primary 5h, secondary weekly} to the current
// {primary weekly, secondary null} layout without a new login, so its auth ID
// stays the same. Replace must drop the old "secondary" window: kept, it would
// roll forward as a full weekly window resetting before the real one and rank
// the credential as if it had 100% left.
func TestQuotaReadingStoreReplaceDropsWindowsALaterSnapshotOmits(t *testing.T) {
	t0 := testNow.Add(-9 * 24 * time.Hour)
	oldLayout := fmt.Sprintf(`{"rate_limit":{`+
		`"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_at":%d},`+
		`"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":%d}}}`,
		t0.Add(2*time.Hour).Unix(), t0.Add(2*time.Hour).Unix())
	newLayout := fmt.Sprintf(`{"rate_limit":{`+
		`"primary_window":{"used_percent":95,"limit_window_seconds":604800,"reset_at":%d},`+
		`"secondary_window":null}}`, testNow.Add(6*24*time.Hour).Unix())
	snapshot := func(body string, at time.Time) []Window {
		t.Helper()
		windows, ok, err := UsageSnapshot("codex", CodexUsageURL, []byte(body), at)
		if err != nil || !ok {
			t.Fatalf("UsageSnapshot = %v, %v, %v", formatWindows(windows), ok, err)
		}
		return windows
	}
	evaluate := func(store *Store) Evaluation {
		return Evaluate(Effective(store, "codex-a", "codex", nil, time.Time{}, testNow), "gpt-5.5", testNow, defaultGate)
	}

	// Put merges, so the stale secondary survives and outranks the real window.
	merged := NewStore()
	merged.Put("codex-a", "codex", snapshot(oldLayout, t0))
	merged.Put("codex-a", "codex", snapshot(newLayout, testNow))
	if got := evaluate(merged); got.RankWindowID != "secondary" || !approxEqual(got.Urgency, 100.0/122) {
		t.Fatalf("merged evaluation = %+v, want the phantom secondary at 100%%/122h", got)
	}

	store := NewStore()
	store.Replace("codex-a", "codex", snapshot(oldLayout, t0), t0)
	store.Replace("codex-a", "codex", snapshot(newLayout, testNow), testNow)
	got := store.Get("codex-a")
	if len(got.Windows) != 1 || got.Windows[0].ID != "primary" || got.Windows[0].Kind != KindLong || got.Windows[0].UsedPercent != 95 {
		t.Fatalf("windows after the layout change = %v, want only the weekly primary", formatWindows(got.Windows))
	}
	if eval := evaluate(store); eval.RankWindowID != "primary" || !approxEqual(eval.Urgency, 5.0/144) {
		t.Fatalf("evaluation = %+v, want the weekly primary at 5%%/144h", eval)
	}
}

func TestQuotaReadingStoreReplace(t *testing.T) {
	t0, t1, t2 := testNow.Add(-2*time.Hour), testNow.Add(-time.Hour), testNow
	week := Window{ID: "7d", Kind: KindLong, UsedPercent: 40, Length: 7 * 24 * time.Hour, Source: SourceUsage}
	short := Window{ID: "5h", Kind: KindShort, UsedPercent: 10, Length: 5 * time.Hour, Source: SourceUsage}
	opus := Window{ID: "7d:opus", Kind: KindScoped, Model: "opus", UsedPercent: 99, Length: 7 * 24 * time.Hour, Source: SourceUsage}
	type put struct {
		replace bool
		at      time.Time
		windows []Window
	}
	tests := []struct {
		name string
		puts []put
		want map[string]float64 // used percent per window ID; nil when the credential is forgotten
	}{
		{
			name: "a scoped window the newer snapshot omits is removed",
			puts: []put{{true, t0, []Window{week, short, opus}}, {true, t1, []Window{withUsage(week, 50, t1), withUsage(short, 20, t1)}}},
			want: map[string]float64{"7d": 50, "5h": 20},
		},
		{
			name: "a window observed after the snapshot is kept",
			puts: []put{{false, t2, []Window{withUsage(opus, 80, t2)}}, {true, t1, []Window{withUsage(week, 50, t1)}}},
			want: map[string]float64{"7d": 50, "7d:opus": 80},
		},
		{
			name: "an older snapshot neither removes nor overrides newer windows",
			puts: []put{{true, t2, []Window{withUsage(week, 60, t2), withUsage(short, 30, t2)}}, {true, t0, []Window{withUsage(week, 5, t0)}}},
			want: map[string]float64{"7d": 60, "5h": 30},
		},
		{
			name: "an empty snapshot forgets the credential",
			puts: []put{{true, t0, []Window{withUsage(week, 40, t0)}}, {true, t1, nil}},
		},
		{
			name: "invalid windows count as not reported",
			puts: []put{{true, t0, []Window{withUsage(week, 40, t0), withUsage(short, 10, t0)}}, {true, t1, []Window{withUsage(week, 50, t1), withUsage(withID(short, ""), 1, t1)}}},
			want: map[string]float64{"7d": 50},
		},
		{
			name: "a first snapshot records like Put",
			puts: []put{{true, t1, []Window{withUsage(week, 40, t1), withUsage(short, 10, t1)}}},
			want: map[string]float64{"7d": 40, "5h": 10},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			for _, p := range tc.puts {
				if p.replace {
					store.Replace("auth-a", "claude", p.windows, p.at)
				} else {
					store.Put("auth-a", "claude", p.windows)
				}
			}
			got := store.Get("auth-a")
			if tc.want == nil {
				if len(got.Windows) != 0 || got.Provider != "" || len(store.Snapshot()) != 0 {
					t.Fatalf("reading = %+v, want the credential forgotten", got)
				}
				return
			}
			used := map[string]float64{}
			for _, w := range got.Windows {
				used[w.ID] = w.UsedPercent
			}
			if got.Provider != "claude" || fmt.Sprint(used) != fmt.Sprint(tc.want) {
				t.Fatalf("reading %q used = %v, want %v", got.Provider, used, tc.want)
			}
		})
	}

	var nilStore *Store
	nilStore.Replace("auth-a", "claude", []Window{withUsage(week, 1, t0)}, t0)
	store := NewStore()
	store.Replace("", "claude", []Window{withUsage(week, 1, t0)}, t0)
	if len(store.Snapshot()) != 0 {
		t.Fatal("Replace recorded a reading without an auth ID")
	}
}

func TestQuotaReadingStoreNilAndDefault(t *testing.T) {
	var nilStore *Store
	nilStore.Put("auth-a", "claude", []Window{{ID: "7d", ObservedAt: testNow}})
	nilStore.Forget("auth-a")
	if got := nilStore.Get("auth-a"); got.AuthID != "auth-a" || got.Windows != nil {
		t.Fatalf("nil store Get = %+v", got)
	}
	if got := nilStore.Snapshot(); got != nil {
		t.Fatalf("nil store Snapshot = %+v", got)
	}
	if Default() == nil || Default() != Default() {
		t.Fatal("Default must return one process-wide store")
	}
}

func TestQuotaReadingStoreConcurrentAccess(t *testing.T) {
	store := NewStore()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			authID := "auth-" + strconv.Itoa(worker%3)
			for i := 0; i < 200; i++ {
				observed := testNow.Add(time.Duration(i) * time.Second)
				store.Put(authID, "claude", []Window{{ID: "7d", Kind: KindLong, UsedPercent: float64(i % 100), ObservedAt: observed}})
				_ = store.Get(authID)
				_ = store.Snapshot()
				_ = Effective(store, authID, "claude", map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5"}, observed, testNow)
			}
		}(worker)
	}
	wg.Wait()
	for _, reading := range store.Snapshot() {
		if len(reading.Windows) != 1 || !reading.Windows[0].ObservedAt.Equal(testNow.Add(199*time.Second)) {
			t.Fatalf("reading %s did not converge on the newest observation: %v", reading.AuthID, formatWindows(reading.Windows))
		}
	}
}

func TestQuotaReadingEffectiveMergesNewestPerWindow(t *testing.T) {
	headerAt := testNow.Add(-2 * time.Minute)
	reset7d := testNow.Add(3 * 24 * time.Hour).Truncate(time.Second)
	reset5h := testNow.Add(2 * time.Hour).Truncate(time.Second)
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.30",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(reset5h.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.60",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(reset7d.Unix(), 10),
	}

	tests := []struct {
		name       string
		stored     []Window
		provider   string
		want5h     float64
		want7d     float64
		wantSource Source
		wantOpus   bool
	}{
		{
			name: "newer usage window beats older headers",
			stored: []Window{
				{ID: "7d", Kind: KindLong, UsedPercent: 64, ResetsAt: reset7d, Length: 7 * 24 * time.Hour, ObservedAt: headerAt.Add(time.Minute), Source: SourceUsage},
				{ID: "7d:opus", Kind: KindScoped, Model: "opus", UsedPercent: 5, Length: 7 * 24 * time.Hour, ObservedAt: headerAt.Add(-time.Hour), Source: SourceUsage},
			},
			provider: "claude",
			want5h:   30, want7d: 64, wantSource: SourceUsage, wantOpus: true,
		},
		{
			name: "newer headers beat an older usage window",
			stored: []Window{
				{ID: "7d", Kind: KindLong, UsedPercent: 64, ResetsAt: reset7d, Length: 7 * 24 * time.Hour, ObservedAt: headerAt.Add(-time.Minute), Source: SourcePoll},
			},
			provider: "claude",
			want5h:   30, want7d: 60, wantSource: SourceHeader,
		},
		{
			name: "headers win a tie",
			stored: []Window{
				{ID: "7d", Kind: KindLong, UsedPercent: 64, ResetsAt: reset7d, Length: 7 * 24 * time.Hour, ObservedAt: headerAt, Source: SourceUsage},
			},
			provider: "claude",
			want5h:   30, want7d: 60, wantSource: SourceHeader,
		},
		{
			name: "stored provider selects the parser when none is given",
			stored: []Window{
				{ID: "7d", Kind: KindLong, UsedPercent: 64, ResetsAt: reset7d, Length: 7 * 24 * time.Hour, ObservedAt: headerAt.Add(-time.Minute), Source: SourceUsage},
			},
			provider: "",
			want5h:   30, want7d: 60, wantSource: SourceHeader,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			store.Put("auth-a", "claude", tc.stored)
			got := Effective(store, "auth-a", tc.provider, signals, headerAt, testNow)
			if got.AuthID != "auth-a" || got.Provider != "claude" {
				t.Fatalf("identity = %q/%q", got.AuthID, got.Provider)
			}
			byID := map[string]Window{}
			for i, w := range got.Windows {
				if i > 0 && got.Windows[i-1].ID >= w.ID {
					t.Fatalf("windows not sorted: %v", formatWindows(got.Windows))
				}
				byID[w.ID] = w
			}
			if byID["5h"].UsedPercent != tc.want5h || byID["7d"].UsedPercent != tc.want7d || byID["7d"].Source != tc.wantSource {
				t.Fatalf("merged = %v", formatWindows(got.Windows))
			}
			if _, ok := byID["7d:opus"]; ok != tc.wantOpus {
				t.Fatalf("opus window present = %v, want %v", ok, tc.wantOpus)
			}
			if stored := store.Get("auth-a"); len(stored.Windows) != len(tc.stored) {
				t.Fatalf("Effective wrote header windows into the store: %v", formatWindows(stored.Windows))
			}
		})
	}
}

func TestQuotaReadingEffectiveNormalizesPassedResets(t *testing.T) {
	week := 7 * 24 * time.Hour
	tests := []struct {
		name       string
		window     Window
		wantUsed   float64
		wantResets time.Time
	}{
		{
			name:       "future reset is untouched",
			window:     Window{ID: "7d", Kind: KindLong, UsedPercent: 90, ResetsAt: testNow.Add(time.Hour), Length: week},
			wantUsed:   90,
			wantResets: testNow.Add(time.Hour),
		},
		{
			name:       "passed reset rolls forward one period",
			window:     Window{ID: "7d", Kind: KindLong, UsedPercent: 90, ResetsAt: testNow.Add(-time.Hour), Length: week},
			wantUsed:   0,
			wantResets: testNow.Add(week - time.Hour),
		},
		{
			name:       "reset exactly now counts as passed",
			window:     Window{ID: "5h", Kind: KindShort, UsedPercent: 100, ResetsAt: testNow, Length: 5 * time.Hour},
			wantUsed:   0,
			wantResets: testNow.Add(5 * time.Hour),
		},
		{
			name:       "several missed periods roll past now",
			window:     Window{ID: "5h", Kind: KindShort, UsedPercent: 100, ResetsAt: testNow.Add(-11 * time.Hour), Length: 5 * time.Hour},
			wantUsed:   0,
			wantResets: testNow.Add(4 * time.Hour),
		},
		{
			name:       "exact multiple of the length still lands after now",
			window:     Window{ID: "5h", Kind: KindShort, UsedPercent: 100, ResetsAt: testNow.Add(-10 * time.Hour), Length: 5 * time.Hour},
			wantUsed:   0,
			wantResets: testNow.Add(5 * time.Hour),
		},
		{
			name:       "unknown length makes the next reset unknown",
			window:     Window{ID: "primary", Kind: KindShort, UsedPercent: 100, ResetsAt: testNow.Add(-time.Minute)},
			wantUsed:   0,
			wantResets: time.Time{},
		},
		{
			name:       "unknown reset is untouched",
			window:     Window{ID: "7d", Kind: KindLong, UsedPercent: 35, Length: week},
			wantUsed:   35,
			wantResets: time.Time{},
		},
		{
			name:       "a tiny length rolls forward without overflow",
			window:     Window{ID: "5h", Kind: KindShort, UsedPercent: 100, ResetsAt: time.Unix(1, 0), Length: time.Nanosecond},
			wantUsed:   0,
			wantResets: testNow.Add(time.Nanosecond),
		},
		{
			name:       "a reset more than a century old becomes unknown",
			window:     Window{ID: "5h", Kind: KindShort, UsedPercent: 100, ResetsAt: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), Length: 5 * time.Hour},
			wantUsed:   0,
			wantResets: time.Time{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			window := tc.window
			window.ObservedAt = testNow.Add(-2 * time.Hour)
			store.Put("auth-a", "claude", []Window{window})
			got := Effective(store, "auth-a", "claude", nil, time.Time{}, testNow)
			if len(got.Windows) != 1 {
				t.Fatalf("windows = %v", formatWindows(got.Windows))
			}
			if got.Windows[0].UsedPercent != tc.wantUsed || !got.Windows[0].ResetsAt.Equal(tc.wantResets) {
				t.Fatalf("normalized = %s, want used %v resets %v", formatWindow(got.Windows[0]), tc.wantUsed, tc.wantResets)
			}
			if stored := store.Get("auth-a").Windows[0]; stored.UsedPercent != tc.window.UsedPercent || !stored.ResetsAt.Equal(tc.window.ResetsAt) {
				t.Fatalf("normalization mutated the store: %s", formatWindow(stored))
			}
		})
	}
}

func TestQuotaReadingEffectiveWithoutStore(t *testing.T) {
	signals := map[string]string{"X-Codex-Primary-Used-Percent": "25", "X-Codex-Primary-Window-Minutes": "300"}
	got := Effective(nil, "auth-x", "codex", signals, testNow, testNow)
	if got.AuthID != "auth-x" || got.Provider != "codex" || len(got.Windows) != 1 || got.Windows[0].UsedPercent != 25 {
		t.Fatalf("Effective(nil store) = %+v", got)
	}
	if empty := Effective(NewStore(), "auth-y", "", nil, time.Time{}, testNow); empty.AuthID != "auth-y" || len(empty.Windows) != 0 {
		t.Fatalf("Effective without data = %+v", empty)
	}
}
