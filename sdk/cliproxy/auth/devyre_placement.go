package auth

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// devyre: what SessionAffinitySelector tells its fallback selector about a
// placement, and the bindings a credential failure suspended. The selector's
// own code calls these from small hunks in selector.go.

// placementActiveWindow is how recently a session must have made a request
// for its binding to count as load on its credential.
const placementActiveWindow = 15 * time.Minute

// maxSuspendedBindings bounds the suspended bindings a selector remembers.
const maxSuspendedBindings = 4096

// placementKind is what the fallback selector places.
type placementKind int

const (
	// placementNew places a session without a binding, or a request without a session.
	placementNew placementKind = iota
	// placementFailover moves a session off the credential it was bound to, which
	// is no longer available to it.
	placementFailover
)

// placement travels in the context from SessionAffinitySelector.Pick to the
// Pick of its fallback selector. Selectors that do not know it ignore it.
type placement struct {
	kind placementKind
	// from is the credential a failover moves the session off.
	from string
	// bindings counts the active bindings per credential for the pick's provider
	// and model; nil outside a failover.
	bindings func() map[string]int
	// reason is set by the fallback selector to why it picked; empty when the
	// fallback does not say.
	reason string
}

type placementContextKey struct{}

// withPlacement returns ctx carrying p.
func withPlacement(ctx context.Context, p *placement) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, placementContextKey{}, p)
}

// placementFrom returns the placement ctx carries, or nil for a pick that did
// not come through a SessionAffinitySelector.
func placementFrom(ctx context.Context) *placement {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(placementContextKey{}).(*placement)
	return p
}

// failover reports whether p moves a session off its credential.
func (p *placement) failover() bool {
	return p != nil && p.kind == placementFailover
}

// logSuffix renders the fallback's reason for a session-affinity log line.
func (p *placement) logSuffix() string {
	if p == nil || p.reason == "" {
		return ""
	}
	return " reason=" + p.reason
}

// devyreFailover returns the placement of a session that leaves from for
// provider and model. It counts the bindings of each credential that were used
// within placementActiveWindow, under the same provider and canonical model key.
func (s *SessionAffinitySelector) devyreFailover(from, provider, model string) *placement {
	p := &placement{kind: placementFailover, from: from}
	if s.cache != nil {
		prefix, suffix := provider+"::", "::"+canonicalModelKey(model)
		p.bindings = func() map[string]int {
			return s.cache.devyreActiveBindings(prefix, suffix, placementActiveWindow, time.Now())
		}
	}
	return p
}

// devyrePlacementFor returns the placement of a session whose cache key has no
// binding: its failover when a credential failure suspended its binding (the
// retry of the request that failed included), a new placement otherwise.
func (s *SessionAffinitySelector) devyrePlacementFor(key, provider, model string, now time.Time) *placement {
	if from, ok := s.devyre.suspendedFor(key, now); ok {
		return s.devyreFailover(from, provider, model)
	}
	return &placement{kind: placementNew}
}

// headroomJudge is a fallback selector that knows whether a credential has
// headroom for a request: ExpiringFirstSelector.
type headroomJudge interface {
	headroomFor(auth *Auth, model string) (bool, string)
}

// devyreJoinsParent reports whether a session may take its parent's binding to
// auth. A fork reads its parent's cached prefix, so staying is what makes its
// cache hit; other aliases of one session stay together too. A non-fork
// subagent starts a fresh context with nothing of its parent's cache to reuse,
// so it takes the parent's credential only while that credential has headroom
// for the request. Otherwise it is placed like a new session, and an info line
// says why. Without a fallback that knows headroom, nothing changes.
func (s *SessionAffinitySelector) devyreJoinsParent(entry *log.Entry, isSubagent bool, auth *Auth, provider, model, sessionID, parentID string) bool {
	judge, ok := s.fallback.(headroomJudge)
	if !isSubagent || !ok {
		return true
	}
	has, reason := judge.headroomFor(auth, model)
	if !has {
		entry.Infof("session-affinity: subagent left its parent's auth without headroom | session=%s parent=%s auth=%s provider=%s model=%s headroom=%q", truncateSessionID(sessionID), truncateSessionID(parentID), auth.ID, provider, model, reason)
	}
	return has
}

// devyreAffinityState is the fork's own state of a SessionAffinitySelector.
type devyreAffinityState struct {
	mu sync.Mutex
	// suspended maps a session's cache key to the binding a credential failure
	// removed (OnResult), until that binding would have expired.
	suspended map[string]suspendedBinding
}

type suspendedBinding struct {
	authID string
	until  time.Time
}

// suspend remembers that a credential failure removed key's binding to authID.
func (d *devyreAffinityState) suspend(key, authID string, until, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.suspended == nil {
		d.suspended = make(map[string]suspendedBinding)
	}
	if _, ok := d.suspended[key]; !ok && len(d.suspended) >= maxSuspendedBindings {
		for other, binding := range d.suspended {
			if !now.Before(binding.until) {
				delete(d.suspended, other)
			}
		}
		if len(d.suspended) >= maxSuspendedBindings {
			d.suspended = make(map[string]suspendedBinding)
		}
	}
	d.suspended[key] = suspendedBinding{authID: authID, until: until}
}

// suspendedFor returns the credential of key's suspended binding, if any.
func (d *devyreAffinityState) suspendedFor(key string, now time.Time) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	binding, ok := d.suspended[key]
	if !ok {
		return "", false
	}
	if !now.Before(binding.until) {
		delete(d.suspended, key)
		return "", false
	}
	return binding.authID, true
}

// forget ends key's suspended binding.
func (d *devyreAffinityState) forget(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.suspended) > 0 {
		delete(d.suspended, key)
	}
}

// CarrySessionBindings copies the live session bindings of the
// SessionAffinitySelector from into the one to, with their absolute expiries,
// and the bindings a credential failure suspended. A selector rebuilt by a
// routing reload then keeps every session where it is: with an empty cache,
// each session's next request would be placed anew, many on another account,
// and pay to write its whole context into that account's cache. It returns how
// many bindings it copied; selectors without session affinity copy nothing.
func CarrySessionBindings(from, to Selector) int {
	src, okFrom := from.(*SessionAffinitySelector)
	dst, okTo := to.(*SessionAffinitySelector)
	if !okFrom || !okTo || src == nil || dst == nil || src == dst || src.cache == nil || dst.cache == nil {
		return 0
	}
	now := time.Now()
	carried := dst.cache.devyreRestore(src.cache.devyreBindings(now), now)
	src.devyre.mu.Lock()
	suspended := make(map[string]suspendedBinding, len(src.devyre.suspended))
	for key, binding := range src.devyre.suspended {
		suspended[key] = binding
	}
	src.devyre.mu.Unlock()
	for key, binding := range suspended {
		if now.Before(binding.until) {
			dst.devyre.suspend(key, binding.authID, binding.until, now)
		}
	}
	return carried
}

// devyreSuspend remembers, after OnResult removed key's binding to authID for a
// credential failure, that the session was bound there: its next pick, the
// retry of the failed request included, is a failover rather than a new
// placement.
func (s *SessionAffinitySelector) devyreSuspend(key, authID string) {
	now := time.Now()
	s.devyre.suspend(key, authID, now.Add(s.cache.ttl), now)
}

// devyreSuspendAuth suspends every binding to authID before InvalidateAuth
// removes them, so those sessions fail over too.
func (s *SessionAffinitySelector) devyreSuspendAuth(authID string) {
	now := time.Now()
	for key, expiresAt := range s.cache.devyreKeysBoundTo(authID, now) {
		s.devyre.suspend(key, authID, expiresAt, now)
	}
}
