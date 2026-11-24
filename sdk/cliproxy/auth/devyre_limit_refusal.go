package auth

// devyre: a request that no credential can take, while each one that takes it again in time is
// held for a limit, is refused as that limit: the model cooldown (429 model_cooldown, with when
// the first hold ends), whatever keeps the other credentials out for good (signed out, an access
// token expired with no refresh to come, turned off).
//
// Upstream refuses with the model cooldown only when every candidate cools down, and otherwise
// with 503 "no auth available". So one signed-out account beside accounts at their limit made a
// usage limit read as a server outage: Claude Code retried the 503 ten times and ended the turn
// with "Claude's API returned a server error", and a client that waits for usage only after a
// limit (Anvil's sessions) left the session failed when the limit lifted. Seen on 2026-11-24: four
// of seven Claude accounts signed out on one computer, the other three at Fable's weekly limit.
//
// A credential that comes back by itself (a transient failure's retry time) is no limit and no
// credential out for good: beside one, the refusal stays the 503 it was.

import "time"

// devyreOutForGood reports whether a candidate the selection blocked takes no request until
// someone acts: nothing it waits for ends at a time, and it is not cooling down.
func devyreOutForGood(reason blockReason, next time.Time) bool {
	return reason != blockReasonCooldown && next.IsZero()
}

// devyreRefusedAsLimit reports whether a request no candidate can take is refused as the limit
// that holds them back: at least one candidate cools down, every other one is out for good
// (devyreOutForGood), and a hold ends at a known time (earliest).
func devyreRefusedAsLimit(total, cooling, outForGood int, earliest time.Time) bool {
	return cooling > 0 && cooling+outForGood == total && !earliest.IsZero()
}

// devyreOutForGoodLocked counts the shard's candidates (those predicate keeps) that take no
// request until someone acts, as the scheduler holds them: turned off, or blocked with no time at
// which what blocks them ends (devyreOutForGood).
func (m *modelScheduler) devyreOutForGoodLocked(predicate func(*scheduledAuth) bool) int {
	if m == nil {
		return 0
	}
	count := 0
	for _, entry := range m.entries {
		if predicate != nil && !predicate(entry) {
			continue
		}
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.state == scheduledStateDisabled ||
			(entry.state == scheduledStateBlocked && entry.nextRetryAt.IsZero()) {
			count++
		}
	}
	return count
}
