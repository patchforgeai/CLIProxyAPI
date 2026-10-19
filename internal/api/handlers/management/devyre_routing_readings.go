package management

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyre: GET /v8/management/routing/quota-readings shows the quota readings
// the expiring-first selector ranks on, for the panel's "next up" hint.

// routingQuotaReadingsResponse is the body of GET /v8/management/routing/quota-readings.
type routingQuotaReadingsResponse struct {
	// Strategy is the normalized configured routing strategy.
	Strategy    string                           `json:"strategy"`
	GeneratedAt time.Time                        `json:"generated_at"`
	Credentials []routingQuotaReadingsCredential `json:"credentials"`
}

// routingQuotaReadingsCredential is one non-disabled credential and its evaluation.
type routingQuotaReadingsCredential struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	// Label is the credential's file name, or its ID when it has no file.
	Label    string `json:"label"`
	Priority int    `json:"priority"`
	// Usable and GateReason report the quota gates, ignoring model-scoped windows.
	Usable     bool   `json:"usable"`
	GateReason string `json:"gate_reason"`
	// Headroom reports whether the credential is usable and every window has
	// more left than its headroom threshold, so new sessions go here before the
	// credentials without it. HeadroomReason names the windows without
	// headroom, such as "5h at 14% left", or the gates; it is left out with
	// headroom.
	Headroom       bool   `json:"headroom"`
	HeadroomReason string `json:"headroom_reason,omitempty"`
	// UrgencyPerHour is the remaining percent per hour until the ranking window
	// resets, rounded to hundredths; null when unknown.
	UrgencyPerHour *float64 `json:"urgency_per_hour"`
	// Rank is the 1-based expiring-first order within the provider, the order a
	// new session is placed in: under that strategy, rank 1 is picked next
	// unless a session binding, a model-scoped gate or the Codex websocket
	// preference applies. A focused credential in the order is rank 1. It is 0
	// for credentials outside that order: gated, unavailable, or below the
	// provider's top available priority tier.
	Rank int `json:"rank"`
	// Focused reports a focus (routingfocus) on the credential; FocusUntil is
	// its end, null without one or when the credential is not focused.
	Focused    bool                         `json:"focused"`
	FocusUntil *time.Time                   `json:"focus_until"`
	Windows    []routingQuotaReadingsWindow `json:"windows"`
}

// routingQuotaReadingsWindow is one effective quota window: resets that already
// passed are rolled forward, so remaining_percent is what is left right now.
type routingQuotaReadingsWindow struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	RemainingPercent float64    `json:"remaining_percent"`
	ResetsAt         *time.Time `json:"resets_at"`
	ObservedAt       *time.Time `json:"observed_at"`
	Source           string     `json:"source"`
}

// GetRoutingQuotaReadings handles GET /v8/management/routing/quota-readings.
// It is read-only and returns identifiers and quota figures, never credentials.
func (h *Handler) GetRoutingQuotaReadings(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	strategy := ""
	expiringFirst := config.ExpiringFirstConfig{}
	if h.cfg != nil {
		strategy = h.cfg.Routing.Strategy
		expiringFirst = h.cfg.Routing.ExpiringFirst
	}
	manager := h.authManager
	h.mu.Unlock()
	thresholds := quotareading.Thresholds{
		Gate:          expiringFirst.GatePercent(),
		HeadroomShort: expiringFirst.HeadroomShort(),
		HeadroomLong:  expiringFirst.HeadroomLong(),
	}

	var auths []*coreauth.Auth
	if manager != nil {
		auths = manager.List()
	}
	c.JSON(http.StatusOK, buildRoutingQuotaReadings(strategy, thresholds, auths, quotareading.Default(), h.devyreFocusStore(), time.Now()))
}

// routingQuotaReadingsEntry carries the ranking inputs next to the response row.
type routingQuotaReadingsEntry struct {
	credential routingQuotaReadingsCredential
	view       coreauth.ExpiringFirstView
	selectable bool
}

// buildRoutingQuotaReadings evaluates every non-disabled credential the way the
// expiring-first selector does (coreauth.ViewExpiringFirst), in a model-agnostic
// view: the empty model ignores model-scoped windows for gating, ranking and
// headroom.
//
// Per provider, the top tier is the highest priority among the credentials the
// selectors would consider (coreauth.AuthSelectable), exactly as the selectors
// narrow candidates. Usable credentials in that tier are ranked in the order
// the selector places a new session: the focused credential first, then
// coreauth.CompareExpiringFirstPlacement, with the auth ID in place of its
// turn-taking among ties.
func buildRoutingQuotaReadings(strategy string, thresholds quotareading.Thresholds, auths []*coreauth.Auth, store *quotareading.Store, focus *routingfocus.Store, now time.Time) routingQuotaReadingsResponse {
	if normalized, ok := normalizeRoutingStrategy(strategy); ok {
		strategy = normalized
	} else {
		strategy = strings.TrimSpace(strategy)
	}
	focused := make(map[string]routingfocus.Focus)
	for _, active := range focus.List(now) {
		focused[active.AuthID] = active
	}

	entries := make([]*routingQuotaReadingsEntry, 0, len(auths))
	topPriority := make(map[string]int)
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		view := coreauth.ViewExpiringFirst(auth, "", store, now, thresholds)
		evaluation := view.Evaluation
		entry := &routingQuotaReadingsEntry{
			credential: routingQuotaReadingsCredential{
				AuthID:         auth.ID,
				AuthIndex:      lockedAuthIndex(auth),
				Provider:       provider,
				Label:          authFileListName(auth),
				Priority:       coreauth.AuthPriority(auth),
				Usable:         evaluation.Usable,
				GateReason:     evaluation.GateReason,
				Headroom:       view.HasHeadroom(),
				HeadroomReason: view.HeadroomReason(),
				Windows:        routingQuotaReadingsWindows(view.Reading.Windows),
			},
			view:       view,
			selectable: coreauth.AuthSelectable(auth, "", now),
		}
		if evaluation.UrgencyKnown {
			urgency := roundHundredths(evaluation.Urgency)
			entry.credential.UrgencyPerHour = &urgency
		}
		if active, ok := focused[auth.ID]; ok {
			entry.credential.Focused = true
			entry.credential.FocusUntil = optionalUTCTime(active.Until)
		}
		if entry.selectable {
			if top, ok := topPriority[provider]; !ok || entry.credential.Priority > top {
				topPriority[provider] = entry.credential.Priority
			}
		}
		entries = append(entries, entry)
	}

	ranked := make(map[string][]*routingQuotaReadingsEntry)
	for _, entry := range entries {
		top, ok := topPriority[entry.credential.Provider]
		if !ok || !entry.selectable || !entry.view.Evaluation.Usable || entry.credential.Priority != top {
			continue
		}
		ranked[entry.credential.Provider] = append(ranked[entry.credential.Provider], entry)
	}
	for _, group := range ranked {
		sort.Slice(group, func(i, j int) bool { return routingQuotaRanksBefore(group[i], group[j]) })
		for position, entry := range group {
			entry.credential.Rank = position + 1
		}
	}

	credentials := make([]routingQuotaReadingsCredential, 0, len(entries))
	for _, entry := range entries {
		credentials = append(credentials, entry.credential)
	}
	sort.Slice(credentials, func(i, j int) bool { return routingQuotaListsBefore(credentials[i], credentials[j]) })
	return routingQuotaReadingsResponse{Strategy: strategy, GeneratedAt: now.UTC(), Credentials: credentials}
}

// routingQuotaRanksBefore orders rank candidates like the selector places a new
// session: the focused credential first, then the placement order, then auth ID
// in place of the selector's round-robin among ties.
func routingQuotaRanksBefore(a, b *routingQuotaReadingsEntry) bool {
	if a.credential.Focused != b.credential.Focused {
		return a.credential.Focused
	}
	if order := coreauth.CompareExpiringFirstPlacement(a.view, b.view); order != 0 {
		return order < 0
	}
	return a.credential.AuthID < b.credential.AuthID
}

// routingQuotaListsBefore orders the response: by provider, ranked credentials
// in rank order, then the rest by label and auth ID.
func routingQuotaListsBefore(a, b routingQuotaReadingsCredential) bool {
	if a.Provider != b.Provider {
		return a.Provider < b.Provider
	}
	if (a.Rank > 0) != (b.Rank > 0) {
		return a.Rank > 0
	}
	if a.Rank != b.Rank {
		return a.Rank < b.Rank
	}
	if labelA, labelB := strings.ToLower(a.Label), strings.ToLower(b.Label); labelA != labelB {
		return labelA < labelB
	}
	return a.AuthID < b.AuthID
}

func routingQuotaReadingsWindows(windows []quotareading.Window) []routingQuotaReadingsWindow {
	out := make([]routingQuotaReadingsWindow, 0, len(windows))
	for _, window := range windows {
		out = append(out, routingQuotaReadingsWindow{
			ID:               window.ID,
			Kind:             window.Kind.String(),
			RemainingPercent: roundHundredths(window.RemainingPercent()),
			ResetsAt:         optionalUTCTime(window.ResetsAt),
			ObservedAt:       optionalUTCTime(window.ObservedAt),
			Source:           string(window.Source),
		})
	}
	return out
}

// optionalUTCTime returns nil for the zero time so it encodes as JSON null.
func optionalUTCTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

// roundHundredths rounds a percentage or rate to two decimals for display.
func roundHundredths(value float64) float64 {
	return math.Round(value*100) / 100
}
