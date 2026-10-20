package cliproxy

import (
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// devyre: session bindings across routing reloads.

// devyreRoutingSelector builds the selector for state, as newRoutingSelector
// does, and carries the session bindings of the current selector over. A
// routing change in a config reload rebuilds the selector, and the old one's
// cache, every binding in it, went with it: each session's next request was
// placed anew, many on another account, which then had to cache the whole
// context again.
func (s *Service) devyreRoutingSelector(state routingRuntimeState) coreauth.Selector {
	selector := newRoutingSelector(state)
	if carried := coreauth.CarrySessionBindings(s.coreManager.Selector(), selector); carried > 0 {
		log.Infof("session-affinity: kept %d bindings across the routing reload", carried)
	}
	return selector
}
