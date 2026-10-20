package auth

import (
	"context"
	"strings"
)

// devyre: a focused credential (internal/routingfocus) takes every request it
// can serve, sessions already bound to another credential included, so its
// weekly quota is burned before it resets. SessionAffinitySelector asks
// devyreFocusTakesOver wherever it would reuse a binding (the explicit-session
// cache hit, the fallback-key cache hit and the LCP match); a yes treats the
// binding as unavailable, so its existing reselect path runs: the fallback
// picks the focus and the session is rebound to it. The session stays there
// afterwards like any binding, also once the focus ends or is gated, until the
// credential becomes unavailable.

// devyreFocusTakesOver reports whether the request of session, bound to
// boundAuthID, should move to a focused credential instead: the fallback is
// the expiring-first selector and would place the request on a usable focus
// among the highest priority tier of available, and boundAuthID is not one of
// them. It returns false at once while nothing is focused.
func (s *SessionAffinitySelector) devyreFocusTakesOver(ctx context.Context, provider, model, session, boundAuthID string, available []*Auth) bool {
	if s == nil {
		return false
	}
	expiring, ok := s.fallback.(*ExpiringFirstSelector)
	if !ok || expiring == nil || expiring.focusStore().Empty() {
		return false
	}
	focused := expiring.usableFocus(ctx, provider, model, highestPriorityAuths(available))
	if len(focused) == 0 {
		return false
	}
	ids := make([]string, 0, len(focused))
	for _, auth := range focused {
		if auth.ID == boundAuthID {
			return false
		}
		ids = append(ids, auth.ID)
	}
	selectorLogEntry(ctx).Infof("session-affinity: focus takes over the binding | session=%s from=%s focus=%s provider=%s model=%s", truncateSessionID(session), boundAuthID, strings.Join(ids, ","), provider, model)
	return true
}
