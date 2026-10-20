package auth

import (
	"sort"
	"strings"
	"time"
)

// devyre: the fork's views of a SessionCache, kept out of session_cache.go.

// sessionBinding is one logical session's binding: its alias group as
// SetAliases made it (Keys[0] keys the group), its credential and its absolute
// expiry.
type sessionBinding struct {
	Keys      []string
	AuthID    string
	ExpiresAt time.Time
}

// devyreBindings returns the bindings live at now, earliest expiry first.
func (c *SessionCache) devyreBindings(now time.Time) []sessionBinding {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	bindings := make([]sessionBinding, 0, len(c.groups))
	for _, group := range c.groups {
		if len(group.aliases) == 0 || group.authID == "" || !now.Before(group.expiresAt) {
			continue
		}
		bindings = append(bindings, sessionBinding{Keys: append([]string(nil), group.aliases...), AuthID: group.authID, ExpiresAt: group.expiresAt})
	}
	sort.Slice(bindings, func(i, j int) bool {
		if !bindings[i].ExpiresAt.Equal(bindings[j].ExpiresAt) {
			return bindings[i].ExpiresAt.Before(bindings[j].ExpiresAt)
		}
		return bindings[i].Keys[0] < bindings[j].Keys[0]
	})
	return bindings
}

// devyreRestore adds the bindings still live at now with their absolute
// expiries, whatever the cache's TTL. A binding whose key is already bound
// keeps the live binding; between restored bindings that share a key, the later
// expiry wins. The earliest expiries are inserted first, so a cache over its
// capacity evicts those. It returns how many bindings it added.
func (c *SessionCache) devyreRestore(bindings []sessionBinding, now time.Time) int {
	if c == nil || len(bindings) == 0 {
		return 0
	}
	latest := append([]sessionBinding(nil), bindings...)
	sort.SliceStable(latest, func(i, j int) bool { return latest[i].ExpiresAt.After(latest[j].ExpiresAt) })

	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureInitializedLocked()
	claimed := make(map[string]struct{})
	accepted := make([]sessionBinding, 0, len(latest))
	for _, binding := range latest {
		keys := compactSessionAliases(mergeSessionAliases(nil, binding.Keys...))
		if binding.AuthID == "" || len(keys) == 0 || !now.Before(binding.ExpiresAt) {
			continue
		}
		free := true
		for _, key := range keys {
			_, taken := claimed[key]
			if entry, ok := c.entries[key]; taken || (ok && now.Before(entry.expiresAt)) {
				free = false
				break
			}
		}
		if !free {
			continue
		}
		for _, key := range keys {
			claimed[key] = struct{}{}
			if expired, ok := c.entries[key]; ok {
				c.removeAliasGroupLocked(expired)
			}
		}
		accepted = append(accepted, sessionBinding{Keys: keys, AuthID: binding.AuthID, ExpiresAt: binding.ExpiresAt})
	}
	for i := len(accepted) - 1; i >= 0; i-- {
		c.replaceAliasGroupsLocked(accepted[i].AuthID, accepted[i].ExpiresAt, accepted[i].Keys)
	}
	return len(accepted)
}

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
