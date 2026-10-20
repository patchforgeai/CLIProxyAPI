package auth

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// devyreTestBinding is a binding of keys to authID expiring at expiresAt.
func devyreTestBinding(authID string, expiresAt time.Time, keys ...string) sessionBinding {
	return sessionBinding{Keys: keys, AuthID: authID, ExpiresAt: expiresAt}
}

func TestDevyreSessionCacheRestore(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := NewSessionCacheWithCapacity(time.Hour, 100)
	defer cache.Stop()
	cache.Set("live", "auth-live")

	restored := cache.devyreRestore([]sessionBinding{
		devyreTestBinding("auth-a", now.Add(30*time.Minute), "s1", "s1-alias"),
		devyreTestBinding("auth-old", now.Add(10*time.Minute), "s1-alias"), // loses the shared key to the later expiry
		devyreTestBinding("auth-b", now.Add(-time.Second), "expired"),
		devyreTestBinding("", now.Add(time.Hour), "no-auth"),
		devyreTestBinding("auth-c", now.Add(time.Hour)),
		devyreTestBinding("auth-other", now.Add(time.Hour), "live"), // the live binding wins
		devyreTestBinding("auth-d", now.Add(2*time.Hour), "s2"),     // beyond the TTL: kept as it is
	}, now)
	if restored != 2 {
		t.Fatalf("restored %d bindings, want 2", restored)
	}
	for key, want := range map[string]string{"s1": "auth-a", "s1-alias": "auth-a", "s2": "auth-d", "live": "auth-live"} {
		if got, ok := cache.Get(key); !ok || got != want {
			t.Fatalf("Get(%s) = %q, %v; want %s", key, got, ok, want)
		}
	}
	for _, key := range []string{"expired", "no-auth"} {
		if got, ok := cache.Get(key); ok {
			t.Fatalf("Get(%s) = %q, want no binding", key, got)
		}
	}
	bindings := cache.devyreBindings(now)
	if len(bindings) != 3 || bindings[0].AuthID != "auth-a" || bindings[1].AuthID != "auth-live" || bindings[2].AuthID != "auth-d" {
		t.Fatalf("bindings = %+v, want auth-a, auth-live, auth-d by expiry", bindings)
	}
	for _, binding := range bindings {
		switch binding.AuthID {
		case "auth-a":
			if !binding.ExpiresAt.Equal(now.Add(30*time.Minute)) || len(binding.Keys) != 2 || binding.Keys[0] != "s1" {
				t.Fatalf("restored binding = %+v, want its own expiry and alias group", binding)
			}
		case "auth-d":
			if !binding.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
				t.Fatalf("restored binding = %+v, want its absolute expiry", binding)
			}
		}
	}

	// Over capacity, the earliest expiries go.
	small := NewSessionCacheWithCapacity(time.Hour, 3)
	defer small.Stop()
	var many []sessionBinding
	for i := 0; i < 5; i++ {
		many = append(many, devyreTestBinding("auth-a", now.Add(time.Duration(i+1)*time.Minute), fmt.Sprintf("k%d", i)))
	}
	small.devyreRestore(many, now)
	if small.Len() != 3 {
		t.Fatalf("Len() = %d, want the capacity of 3", small.Len())
	}
	for i := 0; i < 5; i++ {
		_, ok := small.Get(fmt.Sprintf("k%d", i))
		if ok != (i >= 2) {
			t.Fatalf("k%d kept = %v; want only the three latest expiries", i, ok)
		}
	}
}

// A selector rebuilt by a routing reload keeps every session where it is,
// with the bindings' absolute expiries whatever the new TTL.
func TestDevyreSessionCarryBindings(t *testing.T) {
	t.Parallel()

	from := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	defer from.Stop()
	to := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newHeadroomTestSelector(), TTL: 2 * time.Hour})
	defer to.Stop()
	auths := []*Auth{{ID: "auth-a", Provider: "claude"}, {ID: "auth-b", Provider: "claude"}}
	for _, session := range []string{"one", "two"} {
		if _, err := from.Pick(context.Background(), "claude", "claude-opus-5-5", efSessionOpts(session), auths); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	from.devyre.suspend("claude::claude:failed::claude-opus-5-5", "auth-b", now.Add(time.Hour), now)
	want := from.cache.devyreBindings(now)

	if carried := CarrySessionBindings(from, to); carried != 2 {
		t.Fatalf("CarrySessionBindings() = %d, want 2", carried)
	}
	got := to.cache.devyreBindings(now)
	if len(got) != len(want) {
		t.Fatalf("carried bindings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].AuthID != want[i].AuthID || !got[i].ExpiresAt.Equal(want[i].ExpiresAt) || fmt.Sprint(got[i].Keys) != fmt.Sprint(want[i].Keys) {
			t.Fatalf("carried binding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, session := range []string{"one", "two"} {
		bound, status := from.LookupAffinity("claude", "claude-opus-5-5", "claude:"+session)
		if carried, carriedStatus := to.LookupAffinity("claude", "claude-opus-5-5", "claude:"+session); status != "bound" || carriedStatus != "bound" || carried != bound {
			t.Fatalf("session %s: %s %s before, %s %s after", session, bound, status, carried, carriedStatus)
		}
	}
	if authID, ok := to.devyre.suspendedFor("claude::claude:failed::claude-opus-5-5", now); !ok || authID != "auth-b" {
		t.Fatalf("suspended binding after the carry = %q, %v", authID, ok)
	}

	for _, pair := range [][2]Selector{{from, &RoundRobinSelector{}}, {&RoundRobinSelector{}, to}, {from, from}, {nil, to}} {
		if carried := CarrySessionBindings(pair[0], pair[1]); carried != 0 {
			t.Fatalf("CarrySessionBindings(%T, %T) = %d, want 0", pair[0], pair[1], carried)
		}
	}
}
