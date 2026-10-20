package auth

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// devyre: the expiring-first strategy. It lives in its own file so the fork
// diff stays isolated; the routing wiring is in sdk/cliproxy/service_config.go.

// Reasons logged with every expiring-first pick.
const (
	expiringFirstReasonMostUrgent = "most-urgent"
	expiringFirstReasonNoData     = "no-data"
	expiringFirstReasonAllGated   = "all-gated"
	expiringFirstReasonFocus      = "focus"
)

// expiringFirstMaxRotationKeys bounds the round-robin cursors, like RoundRobinSelector.
const expiringFirstMaxRotationKeys = 4096

// ExpiringFirstSelector prefers the credential whose quota would be lost soonest.
//
// Urgency is the remaining percent of a credential's ranking window (its
// longest window over 24 hours, such as Claude's 7-day limit, else its longest
// window) divided by the hours until that window resets, so quota that is
// about to reset unused is burned first.
// A credential is gated (skipped) while any window that applies to the request
// has at most GateRemainingPercent left and has not reset yet: its short
// windows (24 hours or less), its long windows, and the windows scoped to the
// requested model family. Short and scoped windows only gate; they never rank
// while a long window is known. Readings come from quotareading.Default()
// merged with the credential's response-header signals.
//
// Picks are deterministic given the readings and the clock: usable credentials
// with a known urgency come first, highest urgency wins; then usable
// credentials without readings; and only when every candidate is gated, the
// gated ones whose gates reset first, so upstream 429s and cooldowns take over.
// Equal urgencies, unknowns and equal gate resets rotate round-robin by ID.
// Candidates are first narrowed to the highest available priority tier, so
// priority tiers keep their meaning. Wrapped by SessionAffinitySelector, a
// bound session keeps its credential while it stays available; this selector
// only places new and failed-over sessions.
//
// A focused credential (routingfocus) overrides all of this: while it is among
// the candidates and usable for the request's model, every pick is the focus,
// whatever the urgencies, and SessionAffinitySelector moves bound sessions to
// it (devyreFocusTakesOver). A gated focus, or one outside the candidates'
// priority tier, is ignored and the pick proceeds as above.
type ExpiringFirstSelector struct {
	// GateRemainingPercent is the remaining percent at or below which a short,
	// long or matching model-scoped window gates the credential. The routing
	// config defaults it to 2 (routing.expiring-first.gate-remaining-percent).
	GateRemainingPercent float64
	// LogPicks logs every pick at info level instead of debug.
	LogPicks bool
	// Now is a test hook; nil means time.Now.
	Now func() time.Time
	// Focus holds the focused credentials; nil means routingfocus.Default().
	Focus *routingfocus.Store

	mu sync.Mutex
	rr map[string]string // provider:model -> last picked ID among ties and unknowns
}

// expiringFirstCandidate pairs an available credential with its quota evaluation.
type expiringFirstCandidate struct {
	auth *Auth
	eval quotareading.Evaluation
}

// Pick selects the available credential whose quota would be lost soonest.
func (s *ExpiringFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	picked, _, err := s.pick(ctx, provider, model, auths)
	if err != nil {
		return nil, err
	}
	return picked.auth, nil
}

// pick implements Pick and also returns the logged reason.
func (s *ExpiringFirstSelector) pick(ctx context.Context, provider, model string, auths []*Auth) (expiringFirstCandidate, string, error) {
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return expiringFirstCandidate{}, "", err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	store := quotareading.Default()
	candidates := make([]expiringFirstCandidate, 0, len(available))
	for _, auth := range available {
		if auth == nil {
			continue
		}
		candidates = append(candidates, expiringFirstCandidate{auth: auth, eval: s.evaluate(store, auth, model, now)})
	}
	if len(candidates) == 0 {
		return expiringFirstCandidate{}, "", &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}
	// Candidates usually arrive sorted by ID already; the rotation relies on it.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].auth.ID < candidates[j].auth.ID })

	reason := expiringFirstReasonFocus
	pool := s.focusPool(candidates, now)
	if len(pool) == 0 {
		pool, reason = expiringFirstPool(candidates)
	}
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	picked := s.rotateLocked(key, pool)
	s.mu.Unlock()

	s.logPick(ctx, provider, model, picked, reason)
	return picked, reason, nil
}

func (s *ExpiringFirstSelector) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ExpiringFirstSelector) focusStore() *routingfocus.Store {
	if s.Focus != nil {
		return s.Focus
	}
	return routingfocus.Default()
}

// evaluate gates and ranks auth for model on its effective readings: store
// merged with the credential's response-header signals.
func (s *ExpiringFirstSelector) evaluate(store *quotareading.Store, auth *Auth, model string, now time.Time) quotareading.Evaluation {
	reading := quotareading.Effective(store, auth.ID, auth.Provider, auth.Quota.Signals, auth.Quota.ObservedAt, now)
	return quotareading.Evaluate(reading, model, now, s.GateRemainingPercent)
}

// focusPool returns the focused candidates that are usable for the request, in
// candidate order: normally one, several only in a mixed-provider pick, where
// they rotate like ties. It is nil when no candidate is a usable focus.
func (s *ExpiringFirstSelector) focusPool(candidates []expiringFirstCandidate, now time.Time) []expiringFirstCandidate {
	focus := s.focusStore()
	if focus.Empty() {
		return nil
	}
	var pool []expiringFirstCandidate
	for _, candidate := range candidates {
		if candidate.eval.Usable && focus.Focused(candidate.auth.ID, now) {
			pool = append(pool, candidate)
		}
	}
	return pool
}

// usableFocus returns the auths among a pick's candidates that the pick would
// place this request on because they are focused: auths are what the pick
// receives (one priority tier), narrowed the way pick narrows them. It is nil
// when nothing is focused or no focus is usable for model.
func (s *ExpiringFirstSelector) usableFocus(ctx context.Context, provider, model string, auths []*Auth) []*Auth {
	focus := s.focusStore()
	if focus.Empty() {
		return nil
	}
	now := s.now()
	store := quotareading.Default()
	var usable []*Auth
	for _, auth := range preferCodexWebsocketAuths(ctx, provider, auths) {
		if auth == nil || !focus.Focused(auth.ID, now) {
			continue
		}
		if s.evaluate(store, auth, model, now).Usable {
			usable = append(usable, auth)
		}
	}
	return usable
}

// expiringFirstPool returns the candidates the pick rotates among, in candidate
// order, and why: the usable candidates sharing the highest known urgency; else
// every usable candidate, none of which has an urgency; else the gated
// candidates whose gates reset first.
func expiringFirstPool(candidates []expiringFirstCandidate) ([]expiringFirstCandidate, string) {
	var pool []expiringFirstCandidate
	for _, candidate := range candidates {
		if !candidate.eval.Usable || !candidate.eval.UrgencyKnown {
			continue
		}
		switch {
		case len(pool) == 0 || candidate.eval.Urgency > pool[0].eval.Urgency:
			pool = append(pool[:0], candidate)
		case candidate.eval.Urgency == pool[0].eval.Urgency:
			pool = append(pool, candidate)
		}
	}
	if len(pool) > 0 {
		return pool, expiringFirstReasonMostUrgent
	}

	for _, candidate := range candidates {
		if candidate.eval.Usable {
			pool = append(pool, candidate)
		}
	}
	if len(pool) > 0 {
		return pool, expiringFirstReasonNoData
	}

	for _, candidate := range candidates {
		switch {
		case len(pool) == 0 || candidate.eval.GateResetsAt.Before(pool[0].eval.GateResetsAt):
			pool = append(pool[:0], candidate)
		case candidate.eval.GateResetsAt.Equal(pool[0].eval.GateResetsAt):
			pool = append(pool, candidate)
		}
	}
	return pool, expiringFirstReasonAllGated
}

// rotateLocked picks from a non-empty pool sorted by ID, round-robin: it
// resumes after the previous pick for key the way RoundRobinSelector does
// (successorIndex), so candidates that drop out between picks do not reset the
// rotation. Callers must hold s.mu.
func (s *ExpiringFirstSelector) rotateLocked(key string, pool []expiringFirstCandidate) expiringFirstCandidate {
	if s.rr == nil {
		s.rr = make(map[string]string)
	}
	if _, ok := s.rr[key]; !ok && len(s.rr) >= expiringFirstMaxRotationKeys {
		s.rr = make(map[string]string)
	}
	index := 0
	if last := s.rr[key]; last != "" {
		index = sort.Search(len(pool), func(i int) bool { return pool[i].auth.ID > last })
		if index >= len(pool) {
			index = 0
		}
	}
	picked := pool[index]
	s.rr[key] = picked.auth.ID
	return picked
}

// logPick records a pick at info level when LogPicks is set, otherwise at
// debug. Only identifiers and quota figures are logged, never credentials.
// auth and urgency are repeated in the message because the console formatter
// prints only a fixed set of fields.
func (s *ExpiringFirstSelector) logPick(ctx context.Context, provider, model string, picked expiringFirstCandidate, reason string) {
	level := log.DebugLevel
	if s.LogPicks {
		level = log.InfoLevel
	}
	if !log.IsLevelEnabled(level) {
		return
	}
	urgency := "unknown"
	var urgencyField any = urgency
	if picked.eval.UrgencyKnown {
		rounded := math.Round(picked.eval.Urgency*1000) / 1000
		urgency = strconv.FormatFloat(rounded, 'f', -1, 64)
		urgencyField = rounded
	}
	var message strings.Builder
	fmt.Fprintf(&message, "expiring-first: pick | auth=%s urgency=%s", picked.auth.ID, urgency)
	if picked.eval.RankWindowID != "" {
		fmt.Fprintf(&message, " window=%s", picked.eval.RankWindowID)
	}
	if !picked.eval.Usable {
		fmt.Fprintf(&message, " gate=%q gate_resets_at=%s", picked.eval.GateReason, picked.eval.GateResetsAt.UTC().Format(time.RFC3339))
	}
	selectorLogEntry(ctx).WithFields(log.Fields{
		"provider": provider,
		"model":    model,
		"auth":     picked.auth.ID,
		"urgency":  urgencyField,
		"reason":   reason,
	}).Log(level, message.String())
}
