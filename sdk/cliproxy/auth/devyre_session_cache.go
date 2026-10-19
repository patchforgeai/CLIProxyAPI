package auth

import (
	"strings"
	"time"
)

// devyre: the fork's views of a SessionCache, kept out of session_cache.go.

// devyreActiveBindings counts, per credential, the live bindings whose key
// starts with prefix and ends with suffix and whose session made a request
// within window before now. A binding's last use is its expiry minus the TTL,
// because every use refreshes the expiry to the use plus the TTL. One pass
// over the bindings, under the read lock.
func (c *SessionCache) devyreActiveBindings(prefix, suffix string, window time.Duration, now time.Time) map[string]int {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	// expiresAt - ttl >= now - window, with the expiry still ahead.
	usedSince := now.Add(c.ttl - window)
	counts := make(map[string]int)
	for primaryKey, group := range c.groups {
		if !now.Before(group.expiresAt) || group.expiresAt.Before(usedSince) {
			continue
		}
		if strings.HasPrefix(primaryKey, prefix) && strings.HasSuffix(primaryKey, suffix) {
			counts[group.authID]++
		}
	}
	return counts
}

// devyreKeysBoundTo returns every live key bound to authID with its expiry.
func (c *SessionCache) devyreKeysBoundTo(authID string, now time.Time) map[string]time.Time {
	if c == nil || authID == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make(map[string]time.Time)
	for key, entry := range c.entries {
		if entry.authID == authID && now.Before(entry.expiresAt) {
			keys[key] = entry.expiresAt
		}
	}
	return keys
}
