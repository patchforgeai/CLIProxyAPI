package quotareading

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// minUrgencyHours floors the time to reset so a window about to reset cannot
// produce an unbounded urgency.
const minUrgencyHours = 0.25

// Evaluation is the routing view of one reading for one model at one instant.
type Evaluation struct {
	// Usable is false when any gate applies.
	Usable bool
	// GateReason names the gating windows, e.g. "5h exhausted", "7d exhausted"
	// or "7d:fable exhausted"; it is empty when usable.
	GateReason string
	// GateResetsAt is the latest reset among the gating windows.
	GateResetsAt time.Time
	UrgencyKnown bool
	// Urgency is the remaining percent per hour until the ranking window resets.
	Urgency      float64
	RankWindowID string
}

// Evaluate gates and ranks a reading for model at now. Pass a reading already
// normalized by Effective; Evaluate itself does not roll resets forward.
//
// Gate: any window that applies to the request, with RemainingPercent() <=
// gateRemainingPercent and ResetsAt after now. Short and long windows always
// apply; a scoped window applies when its Model equals ModelFamily(model). An
// exhausted long window therefore gates too: the credential would only answer
// 429 until that window resets.
//
// Ranking window: the long window with the largest Length (ties: the earliest
// known ResetsAt, then the smallest ID). Without a long window, the longest
// window of any kind except scoped windows of a different family, with the
// same tie-breaks.
//
// Urgency: RemainingPercent / max(hours until ResetsAt, 0.25). It is unknown
// when there is no ranking window or its ResetsAt is zero or not after now.
func Evaluate(r Reading, model string, now time.Time, gateRemainingPercent float64) Evaluation {
	family := ModelFamily(model)
	evaluation := Evaluation{Usable: true}

	var gating []string
	for _, window := range r.Windows {
		exhausted := window.RemainingPercent() <= gateRemainingPercent
		if !appliesTo(window, family) || !exhausted || !window.ResetsAt.After(now) {
			continue
		}
		gating = append(gating, window.ID+" exhausted")
		if window.ResetsAt.After(evaluation.GateResetsAt) {
			evaluation.GateResetsAt = window.ResetsAt
		}
	}
	if len(gating) > 0 {
		sort.Strings(gating)
		evaluation.Usable = false
		evaluation.GateReason = strings.Join(gating, ", ")
	}

	rank, ok := rankingWindow(r.Windows, family)
	if !ok {
		return evaluation
	}
	evaluation.RankWindowID = rank.ID
	if rank.ResetsAt.IsZero() || !rank.ResetsAt.After(now) {
		return evaluation
	}
	hours := rank.ResetsAt.Sub(now).Hours()
	if hours < minUrgencyHours {
		hours = minUrgencyHours
	}
	evaluation.Urgency = rank.RemainingPercent() / hours
	evaluation.UrgencyKnown = true
	return evaluation
}

// appliesTo reports whether window limits a request for the model family:
// short and long windows always do, a scoped window only for its own family.
func appliesTo(window Window, family string) bool {
	return window.Kind == KindShort || window.Kind == KindLong ||
		(window.Kind == KindScoped && window.Model != "" && window.Model == family)
}

// Thresholds are the remaining-percent limits of expiring-first routing.
type Thresholds struct {
	// Gate gates a credential: a window that applies to the request has at
	// most this much left and has not reset yet (see Evaluate).
	Gate float64
	// HeadroomShort is what short windows (24 hours or less) need to keep:
	// more than this percent left.
	HeadroomShort float64
	// HeadroomLong is what long windows and the scoped window of the requested
	// family need to keep: more than this percent left.
	HeadroomLong float64
}

// Headroom tells whether a credential's windows leave room for one more
// session.
type Headroom struct {
	// Has is false when a window that applies to the request is at or below
	// its headroom threshold.
	Has bool
	// Reason names the windows without headroom, such as "5h at 14% left";
	// empty when Has.
	Reason string
	// ShortRemaining is the lowest remaining percent among the short windows
	// that count, 100 when none does: what a session placed on the credential
	// can still spend before its short window runs out.
	ShortRemaining float64
}

// EvaluateHeadroom reports whether r leaves room for a request for model at
// now. Pass a reading already normalized by Effective.
//
// A window that applies to the request (the same rule as Evaluate's gates)
// needs more than its threshold left: t.HeadroomShort for a short window,
// t.HeadroomLong for a long window or a scoped window of the requested family.
// A threshold at or below t.Gate (or NaN) turns its check off, so the gate
// alone applies. A window whose reset is unknown or already passed does not
// count, and a reading without windows has headroom.
func EvaluateHeadroom(r Reading, model string, now time.Time, t Thresholds) Headroom {
	family := ModelFamily(model)
	headroom := Headroom{Has: true, ShortRemaining: 100}
	var lacking []string
	for _, window := range r.Windows {
		if !appliesTo(window, family) || window.ResetsAt.IsZero() || !window.ResetsAt.After(now) {
			continue
		}
		remaining := window.RemainingPercent()
		threshold := t.HeadroomLong
		if window.Kind == KindShort {
			threshold = t.HeadroomShort
			headroom.ShortRemaining = math.Min(headroom.ShortRemaining, remaining)
		}
		if !(threshold > t.Gate) || remaining > threshold {
			continue
		}
		lacking = append(lacking, window.ID+" at "+strconv.FormatFloat(math.Round(remaining*10)/10, 'f', -1, 64)+"% left")
	}
	if len(lacking) > 0 {
		sort.Strings(lacking)
		headroom.Has = false
		headroom.Reason = strings.Join(lacking, ", ")
	}
	return headroom
}

// rankingWindow picks the window whose remaining quota is ranked for family.
func rankingWindow(windows []Window, family string) (Window, bool) {
	var best Window
	found := false
	for _, window := range windows {
		if window.Kind == KindLong && (!found || ranksBefore(window, best)) {
			best, found = window, true
		}
	}
	if found {
		return best, true
	}
	for _, window := range windows {
		if window.Kind == KindScoped && window.Model != family {
			continue
		}
		if !found || ranksBefore(window, best) {
			best, found = window, true
		}
	}
	return best, found
}

// ranksBefore orders ranking candidates: longer first, then a known reset
// before an unknown one, then the earlier reset, then the smaller ID.
func ranksBefore(a, b Window) bool {
	if a.Length != b.Length {
		return a.Length > b.Length
	}
	aKnown, bKnown := !a.ResetsAt.IsZero(), !b.ResetsAt.IsZero()
	if aKnown != bKnown {
		return aKnown
	}
	if aKnown && !a.ResetsAt.Equal(b.ResetsAt) {
		return a.ResetsAt.Before(b.ResetsAt)
	}
	return a.ID < b.ID
}
