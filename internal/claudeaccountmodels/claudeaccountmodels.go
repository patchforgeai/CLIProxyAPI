// Package claudeaccountmodels holds the models Anthropic itself offers each
// Claude account (devyre).
//
// Upstream learns which models a Claude credential may use only from the model
// catalog (models.json): embedded in the binary, and replaced every three hours
// by a copy downloaded from a third party's repository. A model Anthropic
// releases is unknown to the router until that third party adds it and the
// next download lands: Claude Fable 5.2 was released on 2026-11-09, the
// catalog listed it at 19:07 UTC that day, and the router refused it ("unknown
// provider for model claude-fable-5-2") and left it out of its own
// GET /v1/models until its next download, while Anthropic served it to every
// account. Clients that build their model menu from that list (Anvil) did not
// offer it at all.
//
// This package keeps, per Claude credential, the list Anthropic answers for
// that account (GET <base>/v1/models with the credential's own token, the
// request the Models API documents): the ids are registered for the
// credential beside the catalog's, and Anthropic's entries describe a model the
// catalog does not know yet (its name, context window, output limit and effort
// levels). The service fills it (sdk/cliproxy/devyre_claude_account_models.go);
// the request paths reach the service through the Learner hook, since they
// cannot import it.
package claudeaccountmodels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// effortLevels are the effort levels the Models API reports, in order.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// Entry is one model of Anthropic's answer, as the Models API describes it.
type Entry struct {
	ID             string         `json:"id"`
	DisplayName    string         `json:"display_name"`
	CreatedAt      string         `json:"created_at"`
	MaxInputTokens int            `json:"max_input_tokens"`
	MaxTokens      int            `json:"max_tokens"`
	Capabilities   map[string]any `json:"capabilities"`
}

// Page is one page of Anthropic's answer.
type Page struct {
	Entries []Entry
	HasMore bool
	LastID  string
}

// Learned is what Anthropic answered for one account.
type Learned struct {
	// At is when it was answered.
	At time.Time
	// Entries are Anthropic's models as listed.
	Entries []Entry
}

// IDs returns the entries' model ids in their listed order.
func (l Learned) IDs() []string {
	out := make([]string, 0, len(l.Entries))
	for _, entry := range l.Entries {
		out = append(out, entry.ID)
	}
	return out
}

// Store keeps the learned lists by credential ID.
type Store struct {
	mu     sync.RWMutex
	byAuth map[string]Learned
}

var defaultStore = NewStore()

// Default returns the store the service fills and the registration reads.
func Default() *Store { return defaultStore }

// NewStore returns an empty store.
func NewStore() *Store { return &Store{byAuth: make(map[string]Learned)} }

// Set records what Anthropic answered for authID and reports whether the
// entries differ from what was recorded before.
func (s *Store) Set(authID string, learned Learned) bool {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, had := s.byAuth[authID]
	s.byAuth[authID] = learned
	return !had || !reflect.DeepEqual(previous.Entries, learned.Entries)
}

// Get returns what was learned for authID.
func (s *Store) Get(authID string) (Learned, bool) {
	if s == nil {
		return Learned{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	learned, ok := s.byAuth[strings.TrimSpace(authID)]
	return learned, ok
}

// Retain forgets every credential not in keep (removed accounts).
func (s *Store) Retain(keep map[string]struct{}) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.byAuth {
		if _, ok := keep[id]; !ok {
			delete(s.byAuth, id)
		}
	}
}

// ParsePage reads one page of Anthropic's GET /v1/models answer
// ({"data":[...],"has_more":bool,"last_id":"..."}), keeping the Claude models.
func ParsePage(body []byte) (Page, error) {
	var payload struct {
		Data    []Entry `json:"data"`
		HasMore bool    `json:"has_more"`
		LastID  string  `json:"last_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &payload); err != nil {
		return Page{}, fmt.Errorf("claude account models: the list is not JSON: %w", err)
	}
	if payload.Data == nil {
		return Page{}, errors.New("claude account models: the list has no data array")
	}
	page := Page{HasMore: payload.HasMore, LastID: strings.TrimSpace(payload.LastID)}
	for _, entry := range payload.Data {
		entry.ID = strings.TrimSpace(entry.ID)
		entry.DisplayName = strings.TrimSpace(entry.DisplayName)
		if !strings.HasPrefix(entry.ID, "claude-") {
			continue
		}
		page.Entries = append(page.Entries, entry)
	}
	return page, nil
}

// Merge joins pages in order, keeping the first entry of each id.
func Merge(pages ...Page) []Entry {
	seen := make(map[string]struct{})
	var out []Entry
	for _, page := range pages {
		for _, entry := range page.Entries {
			key := strings.ToLower(entry.ID)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, entry)
		}
	}
	return out
}

// ModelInfos returns the registry's description of each learned model: the
// catalog's own when it knows the model, else one built from Anthropic's entry.
func ModelInfos(learned Learned) []*registry.ModelInfo {
	out := make([]*registry.ModelInfo, 0, len(learned.Entries))
	for _, entry := range learned.Entries {
		if info := ModelInfo(entry); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// ModelInfo describes one learned model; nil for an entry without an id.
func ModelInfo(entry Entry) *registry.ModelInfo {
	if entry.ID == "" {
		return nil
	}
	if known := registry.LookupStaticModelInfoByChannel(entry.ID, "claude"); known != nil {
		return known
	}
	info := &registry.ModelInfo{
		ID:                        entry.ID,
		Object:                    "model",
		OwnedBy:                   "anthropic",
		Type:                      "claude",
		DisplayName:               entry.DisplayName,
		ContextLength:             entry.MaxInputTokens,
		MaxCompletionTokens:       entry.MaxTokens,
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
	}
	if info.DisplayName == "" {
		info.DisplayName = entry.ID
	}
	if created, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.CreatedAt)); err == nil {
		info.Created = created.Unix()
	}
	if supported(entry.Capabilities, "image_input") {
		info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
	}
	if levels := EffortLevels(entry); len(levels) > 0 {
		info.Thinking = &registry.ThinkingSupport{
			// Adaptive thinking: the model decides; a fixed budget is not taken.
			DynamicAllowed: supported(entry.Capabilities, "thinking", "types", "adaptive"),
			Levels:         levels,
		}
	}
	return info
}

// EffortLevels returns the effort levels Anthropic reports for the model, in
// order; none when it reports no effort support.
func EffortLevels(entry Entry) []string {
	if !supported(entry.Capabilities, "effort") {
		return nil
	}
	levels := make([]string, 0, len(effortLevels))
	for _, level := range effortLevels {
		if supported(entry.Capabilities, "effort", level) {
			levels = append(levels, level)
		}
	}
	return levels
}

// supported reads capabilities[path...].supported.
func supported(capabilities map[string]any, path ...string) bool {
	node := capabilities
	for _, key := range path {
		next, ok := node[key].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	value, _ := node["supported"].(bool)
	return value
}

// Learner is the service side: it asks Anthropic for its Claude accounts'
// lists.
type Learner interface {
	// Ensure learns the accounts' lists before a Claude client is answered a
	// model list: it waits, briefly, for an account that has none yet, and
	// refreshes stale ones behind.
	Ensure(ctx context.Context)
	// Resolve is asked when no credential serves model: it waits, briefly, for
	// a lookup already running or starts one when none ran lately, and reports
	// whether one ended, so the caller looks the model up again.
	Resolve(ctx context.Context, model string) bool
}

type learnerBox struct{ learner Learner }

var currentLearner atomic.Pointer[learnerBox]

// SetLearner installs the service's learner; nil removes it.
func SetLearner(learner Learner) {
	if learner == nil {
		currentLearner.Store(nil)
		return
	}
	currentLearner.Store(&learnerBox{learner: learner})
}

// Ensure asks the installed learner, if any (Learner.Ensure).
func Ensure(ctx context.Context) {
	if box := currentLearner.Load(); box != nil {
		box.learner.Ensure(ctx)
	}
}

// Resolve asks the installed learner, if any (Learner.Resolve).
func Resolve(ctx context.Context, model string) bool {
	if box := currentLearner.Load(); box != nil {
		return box.learner.Resolve(ctx, model)
	}
	return false
}
