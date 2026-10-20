package auth

import (
	"cmp"
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

// Reasons logged with every expiring-first pick: the pool the pick came from.
const (
	expiringFirstReasonMostUrgent  = "most-urgent"
	expiringFirstReasonNoData      = "no-data"
	expiringFirstReasonLowHeadroom = "low-headroom"
	expiringFirstReasonAllGated    = "all-gated"
	expiringFirstReasonFocus       = "focus"
	// A failover ranks both headroom pools by stay power.
	expiringFirstReasonFailoverStayPower   = "failover-stay-power"
	expiringFirstReasonFailoverLowHeadroom = "failover-low-headroom"
	expiringFirstReasonFailoverAllGated    = "failover-all-gated"
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
// while a long window is known. A usable credential has headroom while each of
// those windows has more left than its headroom threshold
// (quotareading.EvaluateHeadroom): a long session placed there does not run it
// out soon after. Readings come from quotareading.Default() merged with the
// credential's response-header signals.
//
// Picks are deterministic given the readings and the clock. They come from the
// first of these pools that is not empty (compareExpiringFirst): usable
// credentials with a known urgency and headroom, highest urgency first; usable
// credentials without readings, which count as having headroom; usable
// credentials without headroom, highest urgency first and unknowns last; and
// only when every candidate is gated, the gated ones whose gates reset first,
// so upstream 429s and cooldowns take over. Ties rotate round-robin by ID.
// Candidates are first narrowed to the highest available priority tier, so
// priority tiers keep their meaning. Wrapped by SessionAffinitySelector, a
// bound session keeps its credential while it stays available; this selector
// only places new and failed-over sessions. A failover (placementFailover)
// ranks the two headroom pools by stay power first: the lowest remaining
// percent of the candidate's short windows divided by one plus its active
// bindings, so sessions that leave one credential together spread over the
// credentials that can carry them instead of all landing on the most urgent.
//
// Before any of this, a credential that failed transiently on the request's model
// a moment ago is left out while another candidate has not
// (devyre_transient_soft.go): the sessions bound there keep it, new ones and
// failovers go elsewhere until it answers or upstream's cooldown would have ended.
//
// A focused credential (routingfocus) overrides all of this: while it is among
// the candidates and usable for the request's model, every pick is the focus,
// whatever the urgencies, headroom or stay power, failovers included, and
// SessionAffinitySelector moves bound sessions to it (devyreFocusTakesOver).
// A gated focus, or one outside the candidates' priority tier, is ignored and
// the pick proceeds as above.
type ExpiringFirstSelector struct {
	// GateRemainingPercent is the remaining percent at or below which a short,
	// long or matching model-scoped window gates the credential. The routing
	// config defaults it to 2 (routing.expiring-first.gate-remaining-percent).
	GateRemainingPercent float64
	// HeadroomShortPercent and HeadroomLongPercent are the headroom thresholds
	// of short windows and of long and matching model-scoped windows. The
	// routing config defaults them to 20 and 5
	// (routing.expiring-first.headroom-short-percent and headroom-long-percent).
	// A threshold at or below GateRemainingPercent, such as the zero value,
	// turns its check off.
	HeadroomShortPercent float64
	HeadroomLongPercent  float64
	// LogPicks logs every pick at info level instead of debug.
	LogPicks bool
	// Now is a test hook; nil means time.Now.
	Now func() time.Time
	// Focus holds the focused credentials; nil means routingfocus.Default().
	Focus *routingfocus.Store
	// transients holds the credentials' recent transient failures
	// (devyre_transient_soft.go); nil means the store the manager shares.
	transients *devyreTransientStore

	mu sync.Mutex
	rr map[string]string // provider:model -> last picked ID among ties and unknowns
}

// expiringFirstCandidate pairs an available credential with its quota
// evaluation and headroom.
type expiringFirstCandidate struct {
	auth     *Auth
	eval     quotareading.Evaluation
	headroom quotareading.Headroom
	// bindings and stayPower are set for failovers: the credential's active
	// bindings, and headroom.ShortRemaining / (1 + bindings).
	bindings  int
	stayPower float64
}

// expiringFirstTier is the pool a candidate is placed from; lower tiers come first.
type expiringFirstTier int

const (
	expiringFirstTierUrgent      expiringFirstTier = iota // usable, known urgency, headroom
	expiringFirstTierNoData                               // usable, unknown urgency, headroom
	expiringFirstTierLowHeadroom                          // usable without headroom
	expiringFirstTierGated                                // gated
)

func (c expiringFirstCandidate) tier() expiringFirstTier {
	switch {
	case !c.eval.Usable:
		return expiringFirstTierGated
	case !c.headroom.Has:
		return expiringFirstTierLowHeadroom
	case c.eval.UrgencyKnown:
		return expiringFirstTierUrgent
	default:
		return expiringFirstTierNoData
	}
}

// compareExpiringFirst is the placement order of the selector and of the
// quota-readings endpoint (CompareExpiringFirstPlacement): negative when a is
// placed before b, positive after, zero when they tie and picks take turns.
// Tiers come first (expiringFirstTier); inside a tier, a higher known urgency
// comes first, except among gated candidates, where the earlier gate reset
// does. A failover ranks the two headroom tiers by stay power before urgency.
func compareExpiringFirst(a, b expiringFirstCandidate, failover bool) int {
	if tierA, tierB := a.tier(), b.tier(); tierA != tierB {
		return cmp.Compare(tierA, tierB)
	}
	switch a.tier() {
	case expiringFirstTierGated:
		return a.eval.GateResetsAt.Compare(b.eval.GateResetsAt)
	case expiringFirstTierUrgent, expiringFirstTierNoData:
		if failover && a.stayPower != b.stayPower {
			return cmp.Compare(b.stayPower, a.stayPower)
		}
	}
	if a.eval.UrgencyKnown != b.eval.UrgencyKnown {
		if a.eval.UrgencyKnown {
			return -1
		}
		return 1
	}
	if !a.eval.UrgencyKnown {
		return 0
	}
	return cmp.Compare(b.eval.Urgency, a.eval.Urgency)
}

// expiringFirstTierReason is the logged reason of a pick from tier.
func expiringFirstTierReason(tier expiringFirstTier, failover bool) string {
	switch {
	case failover && (tier == expiringFirstTierUrgent || tier == expiringFirstTierNoData):
		return expiringFirstReasonFailoverStayPower
	case failover && tier == expiringFirstTierLowHeadroom:
		return expiringFirstReasonFailoverLowHeadroom
	case failover:
		return expiringFirstReasonFailoverAllGated
	case tier == expiringFirstTierUrgent:
		return expiringFirstReasonMostUrgent
	case tier == expiringFirstTierNoData:
		return expiringFirstReasonNoData
	case tier == expiringFirstTierLowHeadroom:
		return expiringFirstReasonLowHeadroom
	default:
		return expiringFirstReasonAllGated
	}
}

// thresholds returns the selector's quota thresholds.
func (s *ExpiringFirstSelector) thresholds() quotareading.Thresholds {
	return quotareading.Thresholds{Gate: s.GateRemainingPercent, HeadroomShort: s.HeadroomShortPercent, HeadroomLong: s.HeadroomLongPercent}
}

// viewExpiringFirst evaluates auth for model at now: its effective reading
// (store merged with its header signals), gates, urgency and headroom.
func viewExpiringFirst(auth *Auth, model string, store *quotareading.Store, now time.Time, thresholds quotareading.Thresholds) ExpiringFirstView {
	reading := quotareading.Effective(store, auth.ID, auth.Provider, auth.Quota.Signals, auth.Quota.ObservedAt, now)
	return ExpiringFirstView{
		Reading:    reading,
		Evaluation: quotareading.Evaluate(reading, model, now, thresholds.Gate),
		Headroom:   quotareading.EvaluateHeadroom(reading, model, now, thresholds),
	}
}

// headroomFor reports whether auth has headroom for a request for model now,
// usable included, and why not (headroomJudge, for subagents joining a parent).
func (s *ExpiringFirstSelector) headroomFor(auth *Auth, model string) (bool, string) {
	view := viewExpiringFirst(auth, model, quotareading.Default(), s.now(), s.thresholds())
	return view.HasHeadroom(), view.HeadroomReason()
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

	place := placementFrom(ctx)
	failover := place.failover()
	var bindings map[string]int
	if failover && place.bindings != nil {
		bindings = place.bindings()
	}

	store := quotareading.Default()
	thresholds := s.thresholds()
	candidates := make([]expiringFirstCandidate, 0, len(available))
	for _, auth := range available {
		if auth == nil {
			continue
		}
		view := viewExpiringFirst(auth, model, store, now, thresholds)
		candidate := expiringFirstCandidate{auth: auth, eval: view.Evaluation, headroom: view.Headroom}
		if failover {
			candidate.bindings = bindings[auth.ID]
			candidate.stayPower = view.Headroom.ShortRemaining / float64(1+candidate.bindings)
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return expiringFirstCandidate{}, "", &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}
	// Candidates usually arrive sorted by ID already; the rotation relies on it.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].auth.ID < candidates[j].auth.ID })
	candidates = s.withoutTransientlyFailing(ctx, provider, model, candidates, now)

	reason := expiringFirstReasonFocus
	pool := s.focusPool(candidates, now)
	if len(pool) == 0 {
		pool, reason = expiringFirstPool(candidates, failover)
	}
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	picked := s.rotateLocked(key, pool)
	s.mu.Unlock()

	if place != nil {
		place.reason = reason
	}
	s.logPick(ctx, provider, model, picked, reason, failover)
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
		// A focus that just failed transiently takes no sessions over: a move there
		// would write their contexts into an account that is failing.
		if _, failing := s.transientStore().avoidedUntil(auth.ID, model, now); failing {
			continue
		}
		if s.evaluate(store, auth, model, now).Usable {
			usable = append(usable, auth)
		}
	}
	return usable
}

func (s *ExpiringFirstSelector) transientStore() *devyreTransientStore {
	if s.transients != nil {
		return s.transients
	}
	return devyreTransientsDefault
}

// withoutTransientlyFailing leaves out the candidates that failed transiently on
// model a moment ago (devyreTransientStore.avoidedUntil), unless every candidate
// did: new sessions and failovers go where the failure is not, while the sessions
// bound to those credentials keep them (devyre_transient_soft.go).
func (s *ExpiringFirstSelector) withoutTransientlyFailing(ctx context.Context, provider, model string, candidates []expiringFirstCandidate, now time.Time) []expiringFirstCandidate {
	store := s.transientStore()
	kept := make([]expiringFirstCandidate, 0, len(candidates))
	var left []string
	for _, candidate := range candidates {
		if _, failing := store.avoidedUntil(candidate.auth.ID, model, now); failing {
			left = append(left, candidate.auth.ID)
			continue
		}
		kept = append(kept, candidate)
	}
	if len(left) == 0 || len(kept) == 0 {
		return candidates
	}
	level := log.DebugLevel
	if s.LogPicks {
		level = log.InfoLevel
	}
	selectorLogEntry(ctx).Logf(level, "expiring-first: left out auths that failed transiently a moment ago | provider=%s model=%s auths=%s", provider, model, strings.Join(left, ","))
	return kept
}

// expiringFirstPool returns the candidates the pick rotates among, in candidate
// order, and why: the candidates that tie for the first place in
// compareExpiringFirst's order, and the reason their tier is logged with.
func expiringFirstPool(candidates []expiringFirstCandidate, failover bool) ([]expiringFirstCandidate, string) {
	var pool []expiringFirstCandidate
	for _, candidate := range candidates {
		if len(pool) == 0 {
			pool = append(pool, candidate)
			continue
		}
		switch order := compareExpiringFirst(candidate, pool[0], failover); {
		case order < 0:
			pool = append(pool[:0], candidate)
		case order == 0:
			pool = append(pool, candidate)
		}
	}
	if len(pool) == 0 {
		return nil, ""
	}
	return pool, expiringFirstTierReason(pool[0].tier(), failover)
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
func (s *ExpiringFirstSelector) logPick(ctx context.Context, provider, model string, picked expiringFirstCandidate, reason string, failover bool) {
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
	if picked.eval.Usable && !picked.headroom.Has {
		fmt.Fprintf(&message, " headroom=%q", picked.headroom.Reason)
	}
	if failover && picked.eval.Usable && picked.headroom.Has {
		stayPower := strconv.FormatFloat(math.Round(picked.stayPower*1000)/1000, 'f', -1, 64)
		fmt.Fprintf(&message, " stay_power=%s bindings=%d", stayPower, picked.bindings)
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
