package quotareading

import (
	"testing"
	"time"
)

// A quota that started over leaves nothing read before it standing: neither
// the stored windows nor the header signals of the rejection itself.
func TestQuotaReadingStoreResetForgetsWhatWasReadBefore(t *testing.T) {
	before, reset, after := testNow.Add(-time.Hour), testNow, testNow.Add(time.Minute)
	week := Window{ID: "7d", Kind: KindLong, UsedPercent: 100, ResetsAt: testNow.Add(72 * time.Hour), Length: claudeWeekWindow, ObservedAt: before, Source: SourceUsage}
	rejected := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
		"Anthropic-Ratelimit-Unified-7d-Reset":  "1791148800",
	}

	store := NewStore()
	store.Put("auth-a", "claude", []Window{week})
	if gated := Evaluate(Effective(store, "auth-a", "claude", rejected, before, reset), "claude-opus-5-5", reset, 2); gated.Usable {
		t.Fatal("precondition: the exhausted reading does not gate the credential")
	}

	store.Reset("auth-a", reset)
	if got := Effective(store, "auth-a", "claude", rejected, before, reset); len(got.Windows) != 0 {
		t.Fatalf("windows read before the reset still stand: %v", formatWindows(got.Windows))
	}
	// A reading that was on its way when the quota started over says nothing either.
	store.Put("auth-a", "claude", []Window{withUsage(week, 100, reset)})
	store.Replace("auth-a", "claude", []Window{withUsage(week, 100, before)}, before)
	if got := store.Get("auth-a"); len(got.Windows) != 0 {
		t.Fatalf("a reading from before the reset was recorded after it: %v", formatWindows(got.Windows))
	}

	// What is read afterwards counts again, from the usage and from headers alike.
	store.Put("auth-a", "claude", []Window{withUsage(week, 3, after)})
	allowed := map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25"}
	got := Effective(store, "auth-a", "claude", allowed, after, after)
	used := map[string]float64{}
	for _, window := range got.Windows {
		used[window.ID] = window.UsedPercent
	}
	if len(used) != 2 || used["7d"] != 3 || used["5h"] != 25 {
		t.Fatalf("readings after the reset = %v, want 7d at 3 and 5h at 25", used)
	}
}

func TestQuotaReadingStoreResetKeepsNewerReadingsAndOtherCredentials(t *testing.T) {
	reset := testNow
	old := Window{ID: "7d", Kind: KindLong, UsedPercent: 100, ObservedAt: reset.Add(-time.Minute), Source: SourceUsage}
	fresh := Window{ID: "5h", Kind: KindShort, UsedPercent: 12, ObservedAt: reset.Add(time.Second), Source: SourceHeader}

	store := NewStore()
	store.Put("auth-a", "claude", []Window{old, fresh})
	store.Put("auth-b", "claude", []Window{old})
	store.Reset("auth-a", reset)
	// An earlier reset than the one recorded changes nothing.
	store.Reset("auth-a", reset.Add(-time.Hour))

	assertWindows(t, store.Get("auth-a").Windows, []Window{fresh})
	assertWindows(t, store.Get("auth-b").Windows, []Window{old})
}

// A credential kept only for its reset is still listed, so the poller forgets
// it with its credential, and Forget drops the reset with the readings.
func TestQuotaReadingStoreResetIsForgottenWithItsCredential(t *testing.T) {
	reset := testNow
	old := Window{ID: "7d", Kind: KindLong, UsedPercent: 100, ObservedAt: reset.Add(-time.Minute), Source: SourceUsage}

	store := NewStore()
	store.Put("auth-a", "claude", []Window{old})
	store.Reset("auth-a", reset)
	snapshot := store.Snapshot()
	if len(snapshot) != 1 || snapshot[0].AuthID != "auth-a" || len(snapshot[0].Windows) != 0 {
		t.Fatalf("snapshot = %+v, want auth-a listed without windows", snapshot)
	}

	store.Forget("auth-a")
	if snapshot = store.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("snapshot after forget = %+v, want none", snapshot)
	}
	// A credential registered later under the same ID starts without a reset.
	store.Put("auth-a", "claude", []Window{old})
	assertWindows(t, store.Get("auth-a").Windows, []Window{old})
}

func TestQuotaReadingStoreResetIgnoresNothingToReset(t *testing.T) {
	var none *Store
	none.Reset("auth-a", testNow)
	store := NewStore()
	store.Reset("", testNow)
	store.Reset("auth-a", time.Time{})
	if snapshot := store.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("snapshot = %+v, want none", snapshot)
	}
}
