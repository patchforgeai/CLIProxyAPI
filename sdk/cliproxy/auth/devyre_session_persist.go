package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// devyre: the session bindings of a SessionAffinitySelector in a file, so a
// restart keeps every session on its credential. The router restarts with the
// app that runs it; with bindings in memory only, each restart placed every
// session anew, and a session placed on another account pays to write its whole
// context into that account's cache.

// SessionAffinityFileName is the file the session bindings persist in.
const SessionAffinityFileName = "session-affinity.json"

const (
	sessionAffinityFileVersion = 1
	// sessionPersistInterval is the shortest time between two saves.
	sessionPersistInterval = 30 * time.Second
	// sessionPersistMaxBytes bounds the file read at start; the cache's
	// capacity keeps a written one far below it.
	sessionPersistMaxBytes = 64 << 20
)

// sessionAffinityFile is the JSON document of SessionAffinityFileName.
type sessionAffinityFile struct {
	Version  int                    `json:"version"`
	Bindings []sessionAffinityEntry `json:"bindings"`
}

// sessionAffinityEntry is one binding: its key, the other keys of its alias
// group, its credential and its absolute expiry.
type sessionAffinityEntry struct {
	Key       string    `json:"key"`
	Aliases   []string  `json:"aliases,omitempty"`
	AuthID    string    `json:"auth_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// persistTimer is the part of *time.Timer a SessionPersistence uses.
type persistTimer interface {
	Stop() bool
}

// SessionPersistence keeps the bindings of one SessionAffinitySelector's cache
// in a file. Load restores them at start, Attach makes every change schedule a
// save, at most one per 30 seconds and only while something changed, and Close
// saves once more. No timer runs while the bindings are saved, and nothing
// outlives Close.
type SessionPersistence struct {
	path     string
	interval time.Duration
	// now and afterFunc are test hooks: time.Now and time.AfterFunc.
	now       func() time.Time
	afterFunc func(time.Duration, func()) persistTimer

	mu      sync.Mutex
	cache   *SessionCache
	dirty   bool
	timer   persistTimer
	closed  bool
	pending sync.WaitGroup // the armed timer's callback
	saveMu  sync.Mutex     // one write at a time
}

// NewSessionPersistence returns the persistence of the bindings in the file at
// path. Nothing runs until Attach.
func NewSessionPersistence(path string) *SessionPersistence {
	return &SessionPersistence{
		path:      path,
		interval:  sessionPersistInterval,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) persistTimer { return time.AfterFunc(d, f) },
	}
}

// Path returns the file the bindings persist in.
func (p *SessionPersistence) Path() string {
	return p.path
}

// sessionCacheOf returns the binding cache of a session affinity selector.
func sessionCacheOf(selector Selector) *SessionCache {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		return affinity.cache
	}
	return nil
}

// Load restores the bindings saved in the file into the cache of selector, a
// SessionAffinitySelector, and returns how many it restored. Expired and
// malformed entries are dropped. A missing file restores nothing; an
// unreadable, corrupt or foreign one is logged and ignored. A restored binding
// is used like a live one: its credential is still checked at every pick, and
// a credential that no longer exists simply fails the session over.
func (p *SessionPersistence) Load(selector Selector) int {
	cache := sessionCacheOf(selector)
	if cache == nil {
		return 0
	}
	saved, err := p.read()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("session-affinity: ignoring %s: %v", p.path, err)
		}
		return 0
	}
	now := p.now()
	bindings := make([]sessionBinding, 0, len(saved.Bindings))
	expired, malformed := 0, 0
	for _, entry := range saved.Bindings {
		keys := append([]string{entry.Key}, entry.Aliases...)
		valid := entry.AuthID != "" && !entry.ExpiresAt.IsZero()
		for _, key := range keys {
			valid = valid && key != ""
		}
		switch {
		case !valid:
			malformed++
		case !now.Before(entry.ExpiresAt):
			expired++
		default:
			bindings = append(bindings, sessionBinding{Keys: keys, AuthID: entry.AuthID, ExpiresAt: entry.ExpiresAt})
		}
	}
	restored := cache.devyreRestore(bindings, now)
	if len(saved.Bindings) > 0 {
		log.Infof("session-affinity: restored %d bindings from %s (%d expired, %d malformed, %d left out)", restored, p.path, expired, malformed, len(bindings)-restored)
	}
	return restored
}

// read decodes the file: a version-1 session-affinity document, at most
// sessionPersistMaxBytes long.
func (p *SessionPersistence) read() (sessionAffinityFile, error) {
	var saved sessionAffinityFile
	file, err := os.Open(p.path)
	if err != nil {
		return saved, err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Debugf("session-affinity: closing %s: %v", p.path, errClose)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, sessionPersistMaxBytes+1))
	if err != nil {
		return saved, err
	}
	if len(data) > sessionPersistMaxBytes {
		return saved, fmt.Errorf("larger than %d bytes", sessionPersistMaxBytes)
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		return saved, fmt.Errorf("not a session-affinity file: %w", err)
	}
	if saved.Version != sessionAffinityFileVersion {
		return saved, fmt.Errorf("unknown session-affinity file version %d", saved.Version)
	}
	return saved, nil
}

// Attach makes the bindings of selector, a SessionAffinitySelector, the ones
// that persist: every change to them schedules a save. It detaches the cache
// attached before; after a switch to another cache a save is scheduled, so
// nothing that changed in between is lost. A selector without session affinity
// detaches. After Close it does nothing.
func (p *SessionPersistence) Attach(selector Selector) {
	cache := sessionCacheOf(selector)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	previous := p.cache
	p.cache = cache
	p.mu.Unlock()
	if previous == cache {
		return
	}
	if previous != nil {
		previous.devyreSetOnChange(nil)
	}
	if cache != nil {
		cache.devyreSetOnChange(p.Changed)
		if previous != nil {
			p.Changed()
		}
	}
}

// Changed schedules a save, as every change to the attached bindings does: it
// arms one timer for the save interval unless one is armed already.
func (p *SessionPersistence) Changed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.cache == nil {
		return
	}
	p.dirty = true
	if p.timer == nil {
		p.pending.Add(1)
		p.timer = p.afterFunc(p.interval, p.saveFromTimer)
	}
}

// saveFromTimer saves the bindings when the timer fires.
func (p *SessionPersistence) saveFromTimer() {
	defer p.pending.Done()
	p.mu.Lock()
	p.timer = nil
	if p.closed || !p.dirty || p.cache == nil {
		p.mu.Unlock()
		return
	}
	p.dirty = false
	cache := p.cache
	p.mu.Unlock()
	if err := p.save(cache); err != nil {
		log.Warnf("session-affinity: saving %s: %v", p.path, err)
		p.mu.Lock()
		p.dirty = true // the next change or Close tries again
		p.mu.Unlock()
	}
}

// Close stops the timer, waits for a save in progress, writes the bindings
// once more when they changed since the last save, and detaches. It is safe to
// call more than once.
func (p *SessionPersistence) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.timer != nil && p.timer.Stop() {
		p.pending.Done()
	}
	p.timer = nil
	cache, dirty := p.cache, p.dirty
	p.cache, p.dirty = nil, false
	p.mu.Unlock()
	if cache != nil {
		cache.devyreSetOnChange(nil)
	}
	p.pending.Wait()
	if cache != nil && dirty {
		if err := p.save(cache); err != nil {
			log.Warnf("session-affinity: saving %s: %v", p.path, err)
		}
	}
}

// save writes the live bindings of cache to the file atomically: a temporary
// file in the same folder, readable by this user only, renamed over it.
func (p *SessionPersistence) save(cache *SessionCache) error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	bindings := cache.devyreBindings(p.now())
	document := sessionAffinityFile{Version: sessionAffinityFileVersion, Bindings: make([]sessionAffinityEntry, 0, len(bindings))}
	for _, binding := range bindings {
		entry := sessionAffinityEntry{Key: binding.Keys[0], AuthID: binding.AuthID, ExpiresAt: binding.ExpiresAt.UTC()}
		if len(binding.Keys) > 1 {
			entry.Aliases = binding.Keys[1:]
		}
		document.Bindings = append(document.Bindings, entry)
	}
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p.path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".session-affinity-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			if errRemove := os.Remove(tempPath); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
				log.Debugf("session-affinity: removing %s: %v", tempPath, errRemove)
			}
		}
	}()
	errWrite := temp.Chmod(0o600)
	if errWrite == nil {
		_, errWrite = temp.Write(data)
	}
	if errWrite == nil {
		errWrite = temp.Sync()
	}
	if errClose := temp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		return errWrite
	}
	if err = os.Rename(tempPath, p.path); err != nil {
		return err
	}
	committed = true
	return nil
}
