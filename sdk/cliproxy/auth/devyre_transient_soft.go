package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// devyre: what a transient upstream failure does to the credential it happened on.
//
// Upstream cools a credential's model for a minute (transientErrorCooldown) after a
// 500, 502, 503, 504 or 529, and the session-affinity selector drops the failing
// session's binding. Both move sessions that did nothing wrong. The conductor leaves a
// cooling credential out before the selector sees it, so every session bound there
// moves on its next request and writes its whole context into another account's
// cache; and a provider with one credential answers every request with a 503 until
// the minute is over (Codex after each OpenAI overload, observed). These failures are
// the provider's (an overload, an edge error), not the credential's.
//
// Under session affinity with expiring-first placement, a credential's transient
// failures on a model are soft for the first minute of a streak
// (devyreSoftTransientWindow, from the first failure since the credential last
// answered on that model; a quiet minute starts a new streak):
//   - nothing cools and no binding is dropped, so the sessions bound there keep the
//     credential and a client's retry reaches it again;
//   - new sessions and failovers are placed elsewhere for as long as upstream would
//     have cooled the credential (ExpiringFirstSelector reads devyreTransientStore),
//     unless every candidate failed too;
//   - a failover attempt that failed puts its session back on the credential its
//     request was bound to, so a request that tries every account during an overload
//     does not leave its session on the last one, away from its cache.
//
// A streak that lasts longer is the credential's own trouble: its failures cool it
// and move its sessions, as upstream does. A success on the model ends the streak.
// Claude Code gives up an Opus request after three 529s in a row through a gateway,
// and other requests after ten failures, so failing over within the request, as
// before, stays: only what a failure does to other sessions and to the bound session
// changes. MarkResult calls devyreSoftTransient first.

// devyreSoftTransientWindow is how long a streak of transient failures stays soft.
const devyreSoftTransientWindow = time.Minute

// devyreMaxTransientEntries bounds the credential and model pairs a store keeps.
const devyreMaxTransientEntries = 4096

// devyreTransientEntry is one credential's streak on one model.
type devyreTransientEntry struct {
	first, last time.Time // the streak's first and latest failure
	avoidUntil  time.Time // new placements go elsewhere until then
}

// devyreTransientStore keeps the streaks of transient failures per credential and
// model, for the conductor (soft or not) and the selector (placed elsewhere).
type devyreTransientStore struct {
	mu      sync.Mutex
	entries map[string]devyreTransientEntry
}

// devyreTransientsDefault is the store the manager and the expiring-first selector
// share, as the quota readings and the focus are shared.
var devyreTransientsDefault = &devyreTransientStore{}

func devyreTransientKey(authID, model string) string {
	return authID + "::" + canonicalModelKey(model)
}

// failed records a transient failure of authID on model at now, after which new
// placements go elsewhere until avoidUntil, and reports whether its streak is
// still soft.
func (s *devyreTransientStore) failed(authID, model string, now, avoidUntil time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]devyreTransientEntry)
	}
	key := devyreTransientKey(authID, model)
	entry, ok := s.entries[key]
	if !ok || now.Sub(entry.last) > devyreSoftTransientWindow {
		if !ok && len(s.entries) >= devyreMaxTransientEntries {
			s.pruneLocked(now)
		}
		entry = devyreTransientEntry{first: now}
	}
	entry.last = now
	if avoidUntil.After(entry.avoidUntil) {
		entry.avoidUntil = avoidUntil
	}
	s.entries[key] = entry
	return now.Sub(entry.first) < devyreSoftTransientWindow
}

// succeeded ends authID's streak on model.
func (s *devyreTransientStore) succeeded(authID, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) > 0 {
		delete(s.entries, devyreTransientKey(authID, model))
	}
}

// avoidedUntil returns until when new placements on model go elsewhere than
// authID, and false when they need not. An empty model asks about any model.
func (s *devyreTransientStore) avoidedUntil(authID, model string, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return time.Time{}, false
	}
	if strings.TrimSpace(model) != "" {
		entry, ok := s.entries[devyreTransientKey(authID, model)]
		return entry.avoidUntil, ok && entry.avoidUntil.After(now)
	}
	var until time.Time
	prefix := authID + "::"
	for key, entry := range s.entries {
		if strings.HasPrefix(key, prefix) && entry.avoidUntil.After(until) {
			until = entry.avoidUntil
		}
	}
	return until, until.After(now)
}

// pruneLocked drops the streaks that are over, and everything when the store is
// still full; the caller holds s.mu.
func (s *devyreTransientStore) pruneLocked(now time.Time) {
	for key, entry := range s.entries {
		if now.Sub(entry.last) > devyreSoftTransientWindow && !entry.avoidUntil.After(now) {
			delete(s.entries, key)
		}
	}
	if len(s.entries) >= devyreMaxTransientEntries {
		s.entries = make(map[string]devyreTransientEntry)
	}
}

// devyreTransients returns the manager's store: its own in tests, else the shared one.
func (m *Manager) devyreTransients() *devyreTransientStore {
	if m != nil && m.devyre.transients != nil {
		return m.devyre.transients
	}
	return devyreTransientsDefault
}

// devyreTransientStatus reports an HTTP status that is a transient upstream
// failure: the ones the bound credential's retries take (devyreTransientFailure).
// Transport failures carry none; upstream already neither cools nor unbinds for them.
func devyreTransientStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	default:
		return false
	}
}

// devyreSoftTransientSelector returns the session-affinity selector when the
// manager places with expiring-first under session affinity, the routing in which
// failures are soft, and nil otherwise.
func (m *Manager) devyreSoftTransientSelector() *SessionAffinitySelector {
	affinity, ok := m.Selector().(*SessionAffinitySelector)
	if !ok || affinity == nil {
		return nil
	}
	if _, ok := affinity.fallback.(*ExpiringFirstSelector); !ok {
		return nil
	}
	return affinity
}

// devyreResultModel is the model a result counts for: the route model the selector
// placed the request with, else the result's own.
func devyreResultModel(result Result) string {
	if model := strings.TrimSpace(result.RouteModel); model != "" {
		return model
	}
	return result.Model
}

// devyreNoteTransient records a transient failure (status) of authID in the store
// and reports whether its streak is still soft. It records nothing, and reports
// false, outside the routing in which failures are soft.
func (m *Manager) devyreNoteTransient(authID, model string, status int, retryAfter *time.Duration) bool {
	if m == nil || authID == "" || !devyreTransientStatus(status) || m.HomeEnabled() || m.devyreSoftTransientSelector() == nil {
		return false
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		return false
	}
	now := time.Now()
	avoidUntil := recoverableFailureRetryAfterWithHint(now, retryAfter, m.cooldownDisabledForAuth(auth))
	return m.devyreTransients().failed(authID, model, now, avoidUntil)
}

// devyreSoftTransient handles result before MarkResult records it. A success ends
// its credential's streak on the model. A transient failure whose streak is still
// soft is recorded without a cooldown or a dropped binding, its session is kept on
// the credential its request was bound to, and true is returned: MarkResult then
// does nothing more. Everything else goes through MarkResult as before.
func (m *Manager) devyreSoftTransient(ctx context.Context, result Result) bool {
	if m == nil || result.AuthID == "" {
		return false
	}
	model := devyreResultModel(result)
	if result.Success {
		m.devyreTransients().succeeded(result.AuthID, model)
		return false
	}
	if result.Error == nil || result.Error.Code == ErrorCodeForceCooldown || result.CredentialScope || shouldSkipCredentialCooldown(result.Error) {
		return false
	}
	status := statusCodeFromResult(result.Error)
	if !m.devyreNoteTransient(result.AuthID, model, status, result.RetryAfter) {
		return false
	}
	m.recordAvailabilityNeutralResult(ctx, result)
	if affinity := m.devyreSoftTransientSelector(); affinity != nil {
		affinity.devyreKeepBoundSession(result)
	}
	session, _ := result.Options.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	logEntryWithRequestID(ctx).Infof("session-affinity: transient failure kept the auth and its sessions | session=%s auth=%s provider=%s model=%s status=%d",
		truncateSessionID(session), result.AuthID, result.Provider, model, status)
	return true
}

// devyreKeepBoundSession puts the session of a failed attempt back on the
// credential its request was bound to (devyreBoundAuthMetadataKey): the attempt's
// pick bound the session to res.AuthID, and a failover that failed must not move
// a session away from its cache. It changes nothing for a request that was not
// bound, that failed on its own credential, or whose session moved on since.
func (s *SessionAffinitySelector) devyreKeepBoundSession(res Result) {
	bound, _ := res.Options.Metadata[devyreBoundAuthMetadataKey].(string)
	if s == nil || s.cache == nil || bound == "" || bound == res.AuthID {
		return
	}
	cacheKey, fallbackKey := s.devyreBindingKeys(res)
	if cacheKey == "" {
		return
	}
	if current, ok := s.cache.Get(cacheKey); !ok || current != res.AuthID {
		return
	}
	if fallbackKey != "" {
		s.cache.SetAliases(bound, cacheKey, fallbackKey)
	} else {
		s.cache.Set(cacheKey, bound)
	}
	s.devyre.forget(cacheKey)
}

// devyreBindingKeys returns the cache keys of res's session binding as OnResult
// computes them: the session's key, and the alias key bound with it (empty when it
// has none). It returns no key for a request without a session and for an LCP
// binding, which lives in the matcher.
func (s *SessionAffinitySelector) devyreBindingKeys(res Result) (string, string) {
	explicitID, explicitFallbackID := extractExplicitSessionIDs(res.Options.Headers, res.Options.OriginalRequest, res.Options.Metadata)
	ns := res.Provider
	if raw, ok := res.Options.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey].(string); ok && raw != "" {
		ns = raw
	}
	nsModel := canonicalModelKey(res.Model)
	if raw, ok := res.Options.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey].(string); ok && raw != "" {
		nsModel = canonicalModelKey(raw)
	}
	if explicitID == "" && s.matcher != nil && res.Options.Metadata != nil {
		if _, isLCP := res.Options.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey]; isLCP {
			return "", ""
		}
	}
	primaryID, fallbackID := explicitID, explicitFallbackID
	if primaryID == "" {
		primaryID, fallbackID = extractSessionIDs(res.Options.Headers, res.Options.OriginalRequest, res.Options.Metadata)
	}
	if primaryID == "" {
		return "", ""
	}
	primaryID = cliproxysession.BoundSessionIdentity(primaryID)
	if fallbackID != "" {
		fallbackID = cliproxysession.BoundSessionIdentity(fallbackID)
	}
	cacheKey := ns + "::" + primaryID + "::" + nsModel
	if fallbackID != "" && fallbackID != primaryID && !isSubagentSession(primaryID, fallbackID) {
		return cacheKey, ns + "::" + fallbackID + "::" + nsModel
	}
	return cacheKey, ""
}
