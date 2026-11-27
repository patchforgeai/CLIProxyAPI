package auth

import (
	"fmt"
	"testing"
	"time"
)

// Six Claude accounts refreshed in the same second, every four hours, for days (Machine, this
// Mac): after one round together they now come due apart, each a stable offset into the lead.
func TestDevyreAccountsRefreshedTogetherComeDueApart(t *testing.T) {
	lead := 4 * time.Hour
	setRefreshLeadFactory(t, "spread-claude", func() *time.Duration {
		d := lead
		return &d
	})
	now := time.Now().Truncate(time.Second)
	expiry := now.Add(8 * time.Hour) // all refreshed in the same second
	due := make(map[time.Time]string)
	for i := 0; i < 7; i++ {
		auth := &Auth{ID: fmt.Sprintf("claude-account-%d@example.com.json", i), Provider: "spread-claude", Metadata: map[string]any{
			"email": "x@example.com", "expires_at": expiry.Format(time.RFC3339),
		}}
		next, ok := nextRefreshCheckAt(now, auth, 15*time.Second)
		if !ok {
			t.Fatal("not scheduled")
		}
		if next.Before(expiry.Add(-lead)) || !next.Before(expiry.Add(-lead/2)) {
			t.Fatalf("%s due at %s, want within the first half of its lead (%s to %s)", auth.ID, next, expiry.Add(-lead), expiry.Add(-lead/2))
		}
		if again, _ := nextRefreshCheckAt(now, auth.Clone(), 15*time.Second); !again.Equal(next) {
			t.Fatalf("%s due at %s, then %s: the offset is not stable", auth.ID, next, again)
		}
		if other, taken := due[next]; taken {
			t.Fatalf("%s and %s come due in the same second", auth.ID, other)
		}
		due[next] = auth.ID
		// shouldRefresh agrees: not yet a second before, due at it.
		manager := NewManager(nil, nil, nil)
		if refreshSpread(auth, lead) > 2*time.Second && manager.shouldRefresh(auth, now) {
			t.Fatalf("%s is due right after its refresh", auth.ID)
		}
	}
}

func TestDevyreTheSpreadStaysInsideHalfTheLead(t *testing.T) {
	for _, lead := range []time.Duration{time.Second, 10 * time.Minute, 4 * time.Hour, 24 * time.Hour} {
		for i := 0; i < 50; i++ {
			spread := refreshSpread(&Auth{ID: fmt.Sprintf("a-%d.json", i)}, lead)
			if spread < 0 || (lead >= 2*time.Second && spread >= lead/2) {
				t.Fatalf("lead %s: spread %s, want under half of it", lead, spread)
			}
		}
	}
}
