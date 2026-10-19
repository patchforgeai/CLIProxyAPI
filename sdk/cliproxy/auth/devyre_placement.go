package auth

import (
	"context"
	"sync"
	"time"
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
