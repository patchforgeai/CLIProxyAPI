package auth

// devyre: refreshes spread out (devyre/README.md, "Refreshes spread out"). A credential is due at
// its access token's expiry minus its provider's lead (Claude's 4 h of an 8 h token). A start or
// a wake makes every credential that came due meanwhile due at once, and since each refresh then
// sets the next one lead before the new expiry, they stayed in step for good: Machine's and this
// Mac's six Claude accounts refreshed in the same second, every four hours, for days, so one
// network blip, stop or 429 met them all. Each credential now comes due a stable offset into its
// lead (from its name), up to half the lead, so their periods differ and they drift apart after
// the first round; at most two refreshes of a provider go out at once (tokenrefresh), and a 429
// holds the provider's next ones.

import (
	"hash/fnv"
	"time"
)

// refreshSpread is how far into its provider's lead a credential comes due: stable for its
// name, under half the lead, in whole seconds.
func refreshSpread(auth *Auth, lead time.Duration) time.Duration {
	if auth == nil || lead < 2*time.Second {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(auth.ID))
	window := uint64((lead / 2) / time.Second)
	return time.Duration(hash.Sum64()%window) * time.Second
}

// spreadLead is the lead a credential is refreshed by: its provider's, less its spread.
func spreadLead(auth *Auth, lead time.Duration) time.Duration {
	return lead - refreshSpread(auth, lead)
}
