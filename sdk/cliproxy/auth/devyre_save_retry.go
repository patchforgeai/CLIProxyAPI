package auth

// devyre: no refresh is lost (devyre/README.md, "No refresh is lost").
//
// A save that fails is tried again until one lands, and once more at shutdown (Flush): before,
// a failed save of rotated tokens was a warning, and the file kept the spent refresh token until
// the next write, four hours later at best; a restart meanwhile loaded the dead token.
//
// The persist guard keeps an older state from overwriting a newer one. Upstream moved its
// watermark for every save asked for, the watcher's skipped ones and the failed ones included:
// a refresh's save that came after a watcher update was dropped without a word. Now the watermark
// is the last write that landed, and a skipped save (the watcher read the file) only says which
// refresh token the newest state holds: an older save that would put back another one is
// dropped, so a refresh never writes its tokens over a new sign-in's.
//
// And the watcher reading back a file that holds a refresh token this router has spent (one
// whose save had not landed yet) no longer puts the dead token back in memory: the credential
// keeps its newer tokens, and they are written again.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// spentKept is how many of a credential's spent refresh tokens are remembered.
const spentKept = 4

// The wait before a failed save is tried again: a second, doubling to a minute (tests shorten it).
var (
	saveRetryBase = time.Second
	saveRetryMax  = time.Minute
)

// persistedState is a credential's state as a save saw it.
type persistedState struct {
	epoch, generation uint64
	refreshToken      string
}

func persistedStateOf(auth *Auth) persistedState {
	return persistedState{epoch: auth.RegistrationEpoch, generation: auth.Generation, refreshToken: tokenDigest(authRefreshToken(auth))}
}

func (s persistedState) before(other persistedState) bool {
	return s.epoch < other.epoch || (s.epoch == other.epoch && s.generation < other.generation)
}

// admits reports whether a save of auth may go to the store (skip: the watcher's, which only
// records what it saw).
func (l *authPersistLock) admits(auth *Auth, skip bool) bool {
	state := persistedStateOf(auth)
	if skip {
		if !state.before(l.known) {
			l.known = state
		}
		return false
	}
	wrote := persistedState{epoch: l.lastEpoch, generation: l.lastGeneration}
	if l.wrote && !wrote.before(state) {
		return false // the store holds this state or a newer one
	}
	if state.before(l.known) && state.refreshToken != l.known.refreshToken {
		return false // it would put back tokens the newest state no longer holds
	}
	return true
}

func (l *authPersistLock) landed(auth *Auth) {
	state := persistedStateOf(auth)
	l.wrote, l.lastEpoch, l.lastGeneration = true, state.epoch, state.generation
	if !state.before(l.known) {
		l.known = state
	}
}

func tokenDigest(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// devyreSaveRetries are the credentials whose last save failed.
type devyreSaveRetries struct {
	mu      sync.Mutex
	pending map[string]*pendingSave
	timer   *time.Timer
	stopped bool
}

type pendingSave struct {
	failures int
	since    time.Time
	next     time.Time
	lastErr  string
}

// devyreNoteSave records how a save of auth went: a failure is tried again until one lands.
func (m *Manager) devyreNoteSave(auth *Auth, err error) {
	if m == nil || auth == nil {
		return
	}
	r := &m.devyre.saves
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		if pending := r.pending[auth.ID]; pending != nil {
			delete(r.pending, auth.ID)
			log.WithFields(log.Fields{"credential": auth.ID, "provider": auth.Provider, "step": "save", "tries": pending.failures + 1}).
				Infof("saved %s after %d failed tries over %s", auth.ID, pending.failures, time.Since(pending.since).Round(time.Second))
		}
		return
	}
	if r.stopped {
		return
	}
	if r.pending == nil {
		r.pending = make(map[string]*pendingSave)
	}
	now := time.Now()
	pending := r.pending[auth.ID]
	if pending == nil {
		pending = &pendingSave{since: now}
		r.pending[auth.ID] = pending
	}
	pending.failures++
	pending.lastErr = err.Error()
	wait := saveRetryBase << min(pending.failures-1, 6)
	pending.next = now.Add(min(wait, saveRetryMax))
	if pending.failures <= 3 || pending.failures%10 == 0 {
		log.WithFields(log.Fields{"credential": auth.ID, "provider": auth.Provider, "step": "save", "tries": pending.failures}).
			Warnf("could not save %s (try %d): %v; trying again at %s", auth.ID, pending.failures, err, pending.next.Format(time.RFC3339))
	}
	m.armSaveRetryLocked(r)
}

func (m *Manager) armSaveRetryLocked(r *devyreSaveRetries) {
	var soonest time.Time
	for _, pending := range r.pending {
		if soonest.IsZero() || pending.next.Before(soonest) {
			soonest = pending.next
		}
	}
	if soonest.IsZero() {
		return
	}
	wait := max(time.Until(soonest), 0)
	if r.timer == nil {
		r.timer = time.AfterFunc(wait, func() { m.retrySaves(false) })
		return
	}
	r.timer.Reset(wait)
}

// retrySaves saves again each credential whose last save failed (all of them with now, else
// those due), as the manager holds it now.
func (m *Manager) retrySaves(now bool) {
	r := &m.devyre.saves
	r.mu.Lock()
	due := make([]string, 0, len(r.pending))
	at := time.Now()
	for id, pending := range r.pending {
		if now || !pending.next.After(at) {
			due = append(due, id)
		}
	}
	r.mu.Unlock()
	for _, id := range due {
		m.mu.RLock()
		auth := m.auths[id].Clone()
		m.mu.RUnlock()
		if auth == nil {
			r.mu.Lock()
			delete(r.pending, id)
			r.mu.Unlock()
			continue
		}
		_ = m.persist(context.Background(), auth)
	}
	r.mu.Lock()
	if !r.stopped {
		m.armSaveRetryLocked(r)
	}
	r.mu.Unlock()
}

// FlushPendingSaves tries once more, now, every save that has not landed, and stops trying
// after; it returns the credentials still unsaved. For a shutdown.
func (m *Manager) FlushPendingSaves() []string {
	if m == nil {
		return nil
	}
	m.retrySaves(true)
	r := &m.devyre.saves
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	if r.timer != nil {
		r.timer.Stop()
	}
	unsaved := make([]string, 0, len(r.pending))
	for id, pending := range r.pending {
		unsaved = append(unsaved, id)
		log.WithFields(log.Fields{"credential": id, "step": "save", "tries": pending.failures}).
			Warnf("%s is not saved at shutdown: %s", id, pending.lastErr)
	}
	return unsaved
}

// noteSpentRefreshToken remembers that a refresh of id spent token.
func (m *Manager) noteSpentRefreshToken(id, token string) {
	if m == nil || id == "" || token == "" {
		return
	}
	digest := tokenDigest(token)
	value, _ := m.devyre.spent.LoadOrStore(id, &spentTokens{})
	spent := value.(*spentTokens)
	spent.mu.Lock()
	defer spent.mu.Unlock()
	spent.digests = append(spent.digests, digest)
	if len(spent.digests) > spentKept {
		spent.digests = spent.digests[len(spent.digests)-spentKept:]
	}
}

func (m *Manager) spentRefreshToken(id, token string) bool {
	if m == nil || token == "" {
		return false
	}
	value, ok := m.devyre.spent.Load(id)
	if !ok {
		return false
	}
	spent := value.(*spentTokens)
	digest := tokenDigest(token)
	spent.mu.Lock()
	defer spent.mu.Unlock()
	for _, each := range spent.digests {
		if each == digest {
			return true
		}
	}
	return false
}

type spentTokens struct {
	mu      sync.Mutex
	digests []string
}

// keepUnspentTokens, for a replacement the watcher read from the file (skip), puts existing's
// tokens back when the file holds a refresh token this router has spent: the file is older than
// the credential (its save had not landed yet). It reports whether it did, for the caller to
// write the credential again.
func (m *Manager) keepUnspentTokens(existing, replacement *Auth) bool {
	if existing == nil || replacement == nil || !m.spentRefreshToken(existing.ID, authRefreshToken(replacement)) ||
		authRefreshToken(existing) == authRefreshToken(replacement) {
		return false
	}
	if replacement.Metadata == nil {
		replacement.Metadata = make(map[string]any)
	}
	for key := range replacement.Metadata {
		if _, kept := existing.Metadata[key]; !kept && IsAuthTokenPayloadKey(key) {
			delete(replacement.Metadata, key)
		}
	}
	for key, value := range existing.Metadata {
		if IsAuthTokenPayloadKey(key) {
			replacement.Metadata[key] = value
		}
	}
	log.WithFields(log.Fields{"credential": existing.ID, "provider": existing.Provider, "step": "save"}).
		Warnf("the token file of %s holds a refresh token already spent: its newer tokens are kept and written again", existing.ID)
	return true
}

// withPersist undoes WithSkipPersist.
func withPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipPersistContextKey{}, false)
}
