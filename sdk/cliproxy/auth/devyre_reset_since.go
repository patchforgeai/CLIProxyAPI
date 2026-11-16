package auth

// devyre: a cooldown reset learned late leaves standing a rejection made since.
//
// A client may say when a credential's limits started over (the time its provider granted a
// reset), not only that they did. A reset used on one computer reaches the routers of the
// account's other computers as soon as they can be told, which for one that was away is when it
// comes back: by then its credential may have run out again, and the rejection that says so is
// newer than the reset. DevyreRejectedSince tells such a rejection apart, so that the reset clears
// only the holds from before it.

import (
	"net/http"
	"strings"
	"time"
)

// DevyreRejectedSince reports whether the provider refused one of the credential's requests on its
// quota after since: a model's own last result is a 429 recorded after it, with its quota cooldown
// still in place. A credential the manager does not hold reports false.
func (m *Manager) DevyreRejectedSince(authID string, since time.Time) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[strings.TrimSpace(authID)]
	if auth == nil {
		return false
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded && state.LastError != nil &&
			state.LastError.HTTPStatus == http.StatusTooManyRequests && state.UpdatedAt.After(since) {
			return true
		}
	}
	return false
}

// quotaKeptRejectedSince is why a cooldown reset that names when the limits
// started over left a hold standing: a rejection of the credential since.
const quotaKeptRejectedSince = "it was refused for its quota after its limits started over"

// DevyreLogResetKept says that a cooldown reset left the credential's holds as they were: the
// provider refused it on its quota after its limits started over.
func DevyreLogResetKept(auth *Auth) {
	devyreLogQuotaKept(auth, quotaKeptRejectedSince, time.Now())
}
