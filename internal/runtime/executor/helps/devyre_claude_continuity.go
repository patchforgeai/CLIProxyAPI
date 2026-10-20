package helps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// devyre: the continuity of every cloaked Claude conversation in a file, so a
// restart keeps it.
//
// A cloaked request stamps the date of its conversation's first request into a
// currentDate reminder at the start of the conversation, and every later
// request of the conversation keeps that date (PinClaudeSessionDate): a new
// date would rewrite the conversation's first message, and with it the whole
// prompt cache in front of the conversation. The pins lived in memory only, and
// the router restarts with the app that runs it: a restart on a later day than
// a conversation's first request pinned the new date, and the conversation's
// next request wrote its whole context to the cache again. The file keeps each
// conversation's pin with the rest of its continuity (its previous message and
// request, its prompt id) for as long as memory keeps them: an hour after the
// conversation's last request, the life of the cache they protect.

// ClaudeContinuityFileName is the file the continuity persists in.
const ClaudeContinuityFileName = "claude-continuity.json"

const (
	claudeContinuityFileVersion = 1
	// claudeContinuityPersistInterval is the shortest time between two saves.
	claudeContinuityPersistInterval = 30 * time.Second
	// claudeContinuityMaxBytes bounds the file read at start; the state's
	// capacity keeps a written one far below it.
	claudeContinuityMaxBytes = 16 << 20
	// claudeContinuityMaxMessageID bounds a restored message ID.
	claudeContinuityMaxMessageID = 256
)

var (
	claudeContinuityKeyPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	claudeContinuityDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// claudeContinuityFile is the JSON document of ClaudeContinuityFileName.
type claudeContinuityFile struct {
	Version       int                         `json:"version"`
	Conversations []claudeContinuityFileEntry `json:"conversations"`
}

// claudeContinuityFileEntry is one conversation's continuity under its key in
// memory (a digest of its credential and conversation), with its absolute
// expiry.
type claudeContinuityFileEntry struct {
	Key               string    `json:"key"`
	PreviousMessageID string    `json:"previous_message_id,omitempty"`
	PreviousRequestID string    `json:"previous_request_id,omitempty"`
	PromptID          string    `json:"prompt_id,omitempty"`
	PinnedDate        string    `json:"pinned_date,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// continuityTimer is the part of *time.Timer a ClaudeContinuityPersistence uses.
type continuityTimer interface {
	Stop() bool
}

// ClaudeContinuityPersistence keeps the continuity of the cloaked Claude
// conversations in a file. Load restores it at start, Attach makes every change
// schedule a save, at most one per 30 seconds and only while something
// changed, and Close saves once more. No timer runs while the continuity is
// saved, and nothing outlives Close.
type ClaudeContinuityPersistence struct {
	path     string
	interval time.Duration
	// now and afterFunc are test hooks: time.Now and time.AfterFunc.
	now       func() time.Time
	afterFunc func(time.Duration, func()) continuityTimer

	mu       sync.Mutex
	attached bool
	dirty    bool
	timer    continuityTimer
	closed   bool
	pending  sync.WaitGroup // the armed timer's callback
	saveMu   sync.Mutex     // one write at a time
}

// NewClaudeContinuityPersistence returns the persistence of the continuity in
// the file at path. Nothing runs until Attach.
func NewClaudeContinuityPersistence(path string) *ClaudeContinuityPersistence {
	return &ClaudeContinuityPersistence{
		path:      path,
		interval:  claudeContinuityPersistInterval,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) continuityTimer { return time.AfterFunc(d, f) },
	}
}

// Path returns the file the continuity persists in.
func (p *ClaudeContinuityPersistence) Path() string {
	return p.path
}

// devyreContinuityChangedLocked tells the attached persistence that the
// continuity changed. Callers hold claudeDiagnosticsState.
func devyreContinuityChangedLocked() {
	if p := claudeDiagnosticsState.devyrePersistence; p != nil {
		p.Changed()
	}
}

// Load restores the continuity saved in the file and returns how many
// conversations it restored. Expired and malformed entries are dropped, and a
// conversation already live in memory keeps its own. A missing file restores
// nothing; an unreadable, corrupt or foreign one is logged and ignored.
func (p *ClaudeContinuityPersistence) Load() int {
	saved, err := p.read()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("claude-continuity: ignoring %s: %v", p.path, err)
		}
		return 0
	}
	now := p.now()
	valid := make([]claudeContinuityFileEntry, 0, len(saved.Conversations))
	expired, malformed := 0, 0
	for _, entry := range saved.Conversations {
		switch {
		case !claudeContinuityEntryValid(entry):
			malformed++
		case !now.Before(entry.ExpiresAt):
			expired++
		default:
			valid = append(valid, entry)
		}
	}
	restored := restoreClaudeContinuity(valid)
	if len(saved.Conversations) > 0 {
		log.Infof("claude-continuity: restored %d conversations from %s (%d expired, %d malformed, %d left out)", restored, p.path, expired, malformed, len(valid)-restored)
	}
	return restored
}

// claudeContinuityEntryValid reports whether a saved entry holds what the live
// state would: a key, an expiry, and only well-formed values.
func claudeContinuityEntryValid(entry claudeContinuityFileEntry) bool {
	if !claudeContinuityKeyPattern.MatchString(entry.Key) || entry.ExpiresAt.IsZero() {
		return false
	}
	if entry.PinnedDate != "" {
		if !claudeContinuityDatePattern.MatchString(entry.PinnedDate) {
			return false
		}
		if _, err := time.Parse("2006-01-02", entry.PinnedDate); err != nil {
			return false
		}
	}
	if entry.PromptID != "" && !IsValidClaudePromptID(entry.PromptID) {
		return false
	}
	if entry.PreviousRequestID != "" && !claudeRequestIDPattern.MatchString(entry.PreviousRequestID) {
		return false
	}
	return len(entry.PreviousMessageID) <= claudeContinuityMaxMessageID
}

// restoreClaudeContinuity adds saved conversations, least recently used first,
// to the live state and returns how many it added. A conversation already live
// keeps its own entry, a key saved twice counts once, and the capacity keeps
// the most recently used. Sequences start over with the process, so a restored
// conversation accepts the commits of this one.
func restoreClaudeContinuity(entries []claudeContinuityFileEntry) int {
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	room := claudeDiagnosticsMaxEntries - len(claudeDiagnosticsState.entries)
	seen := make(map[string]bool, len(entries))
	picked := make([]claudeContinuityFileEntry, 0, min(len(entries), max(room, 0)))
	for i := len(entries) - 1; i >= 0 && len(picked) < room; i-- {
		entry := entries[i]
		if seen[entry.Key] {
			continue
		}
		seen[entry.Key] = true
		if _, live := claudeDiagnosticsState.entries[entry.Key]; live {
			continue
		}
		picked = append(picked, entry)
	}
	for i := len(picked) - 1; i >= 0; i-- {
		entry := picked[i]
		claudeDiagnosticsState.nextAccess++
		claudeDiagnosticsState.entries[entry.Key] = claudeDiagnosticsEntry{
			previousMessageID: entry.PreviousMessageID,
			previousRequestID: entry.PreviousRequestID,
			promptID:          entry.PromptID,
			pinnedDate:        entry.PinnedDate,
			lastAccess:        claudeDiagnosticsState.nextAccess,
			expiresAt:         entry.ExpiresAt,
		}
	}
	return len(picked)
}

// claudeContinuitySnapshot returns the live conversations, least recently used first.
func claudeContinuitySnapshot(now time.Time) []claudeContinuityFileEntry {
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	type ranked struct {
		access uint64
		entry  claudeContinuityFileEntry
	}
	live := make([]ranked, 0, len(claudeDiagnosticsState.entries))
	for key, entry := range claudeDiagnosticsState.entries {
		if entry.expiresAt.IsZero() || !now.Before(entry.expiresAt) {
			continue
		}
		live = append(live, ranked{access: entry.lastAccess, entry: claudeContinuityFileEntry{
			Key:               key,
			PreviousMessageID: entry.previousMessageID,
			PreviousRequestID: entry.previousRequestID,
			PromptID:          entry.promptID,
			PinnedDate:        entry.pinnedDate,
			ExpiresAt:         entry.expiresAt.UTC(),
		}})
	}
	sort.Slice(live, func(i, j int) bool { return live[i].access < live[j].access })
	out := make([]claudeContinuityFileEntry, len(live))
	for i, r := range live {
		out[i] = r.entry
	}
	return out
}

// read decodes the file: a version-1 continuity document, at most
// claudeContinuityMaxBytes long.
func (p *ClaudeContinuityPersistence) read() (claudeContinuityFile, error) {
	var saved claudeContinuityFile
	file, err := os.Open(p.path)
	if err != nil {
		return saved, err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Debugf("claude-continuity: closing %s: %v", p.path, errClose)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, claudeContinuityMaxBytes+1))
	if err != nil {
		return saved, err
	}
	if len(data) > claudeContinuityMaxBytes {
		return saved, fmt.Errorf("larger than %d bytes", claudeContinuityMaxBytes)
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		return saved, fmt.Errorf("not a claude-continuity file: %w", err)
	}
	if saved.Version != claudeContinuityFileVersion {
		return saved, fmt.Errorf("unknown claude-continuity file version %d", saved.Version)
	}
	return saved, nil
}

// Attach makes every change to the continuity schedule a save, in place of the
// persistence attached before. After Close it does nothing.
func (p *ClaudeContinuityPersistence) Attach() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.attached = true
	p.mu.Unlock()
	claudeDiagnosticsState.Lock()
	claudeDiagnosticsState.devyrePersistence = p
	claudeDiagnosticsState.Unlock()
}

// Changed schedules a save, as every change to the continuity does once
// attached: it arms one timer for the save interval unless one is armed
// already.
func (p *ClaudeContinuityPersistence) Changed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.attached {
		return
	}
	p.dirty = true
	if p.timer == nil {
		p.pending.Add(1)
		p.timer = p.afterFunc(p.interval, p.saveFromTimer)
	}
}

// saveFromTimer saves the continuity when the timer fires.
func (p *ClaudeContinuityPersistence) saveFromTimer() {
	defer p.pending.Done()
	p.mu.Lock()
	p.timer = nil
	if p.closed || !p.dirty {
		p.mu.Unlock()
		return
	}
	p.dirty = false
	p.mu.Unlock()
	if err := p.save(); err != nil {
		log.Warnf("claude-continuity: saving %s: %v", p.path, err)
		p.mu.Lock()
		p.dirty = true // the next change or Close tries again
		p.mu.Unlock()
	}
}

// Close stops the timer, detaches, waits for a save in progress, and writes the
// continuity once more when it changed since the last save. It is safe to call
// more than once.
func (p *ClaudeContinuityPersistence) Close() {
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
	attached, dirty := p.attached, p.dirty
	p.attached, p.dirty = false, false
	p.mu.Unlock()
	if attached {
		claudeDiagnosticsState.Lock()
		if claudeDiagnosticsState.devyrePersistence == p {
			claudeDiagnosticsState.devyrePersistence = nil
		}
		claudeDiagnosticsState.Unlock()
	}
	p.pending.Wait()
	if attached && dirty {
		if err := p.save(); err != nil {
			log.Warnf("claude-continuity: saving %s: %v", p.path, err)
		}
	}
}

// save writes the live continuity to the file atomically: a temporary file in
// the same folder, readable by this user only, renamed over it.
func (p *ClaudeContinuityPersistence) save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	data, err := json.Marshal(claudeContinuityFile{Version: claudeContinuityFileVersion, Conversations: claudeContinuitySnapshot(p.now())})
	if err != nil {
		return err
	}
	dir := filepath.Dir(p.path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".claude-continuity-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			if errRemove := os.Remove(tempPath); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
				log.Debugf("claude-continuity: removing %s: %v", tempPath, errRemove)
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
