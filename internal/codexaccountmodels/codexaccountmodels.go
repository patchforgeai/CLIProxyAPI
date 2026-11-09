// Package codexaccountmodels holds the models OpenAI itself offers each Codex
// account (devyre).
//
// Upstream learns which models a Codex account may use only from the model
// catalog (models.json): embedded in the binary, and replaced at runtime by a
// copy downloaded from a third party's repository. A model OpenAI released after
// the build is unknown until that download succeeds, so a router that cannot
// reach it (a filtered network, a failing mirror) refuses the model with
// "unknown provider for model" while Codex itself serves it.
//
// This package keeps, per Codex OAuth credential, the catalog OpenAI answers for
// that account (GET <codex base>/models?client_version=<v>, the request Codex
// makes): the slugs are registered for the credential beside the catalog's, and
// the entries, which are OpenAI's own Codex client metadata, describe a model
// the Codex client catalog has no template for. The service fills it
// (sdk/cliproxy/devyre_codex_account_models.go); the request paths reach the
// service through the Learner hook, since they cannot import it.
package codexaccountmodels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// FloorClientVersion is the Codex client version asked for before any Codex
// client has said which version it is. OpenAI filters its catalog by the
// version (an older client is not offered a newer model), so the newest version
// a client names replaces it (Learner.Ensure).
const FloorClientVersion = "0.163.0"

// orchestrationEffort is Codex's proactive multi-agent level. The catalog
// leaves it out of a model's thinking levels: it is the client's mode, not a
// reasoning effort the upstream request carries.
const orchestrationEffort = "ultra"

// Learned is what OpenAI answered for one account.
type Learned struct {
	// ClientVersion is the Codex client version the catalog was asked for.
	ClientVersion string
	// At is when it was answered.
	At time.Time
	// Entries are OpenAI's catalog entries as listed, each with a slug.
	Entries []map[string]any
}

// Slugs returns the entries' model ids in their listed order.
func (l Learned) Slugs() []string {
	out := make([]string, 0, len(l.Entries))
	for _, entry := range l.Entries {
		if slug := entrySlug(entry); slug != "" {
			out = append(out, slug)
		}
	}
	return out
}

// Store keeps the learned catalogs by credential ID.
type Store struct {
	mu     sync.RWMutex
	byAuth map[string]Learned
}

var defaultStore = NewStore()

// Default returns the store the service fills and the request paths read.
func Default() *Store { return defaultStore }

// NewStore returns an empty store.
func NewStore() *Store { return &Store{byAuth: make(map[string]Learned)} }

// Set records what OpenAI answered for authID and reports whether the entries
// differ from what was recorded before.
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

// Templates returns OpenAI's entry for each learned model whose entry is a
// complete Codex client catalog entry, by slug. When accounts disagree, the one
// asked for the newest client version wins, then the most recent answer, then
// the lowest credential ID, so the result does not depend on map order.
func (s *Store) Templates() map[string]map[string]any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.byAuth))
	for id := range s.byAuth {
		ids = append(ids, id)
	}
	learned := make(map[string]Learned, len(s.byAuth))
	for id, l := range s.byAuth {
		learned[id] = l
	}
	s.mu.RUnlock()
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := learned[ids[i]], learned[ids[j]]
		if c := CompareVersions(a.ClientVersion, b.ClientVersion); c != 0 {
			return c > 0
		}
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return ids[i] < ids[j]
	})
	out := make(map[string]map[string]any)
	for _, id := range ids {
		for _, entry := range learned[id].Entries {
			slug := entrySlug(entry)
			if slug == "" {
				continue
			}
			if _, seen := out[slug]; seen {
				continue
			}
			if !IsCompleteClientEntry(entry) {
				continue
			}
			out[slug] = entry
		}
	}
	return out
}

// ParseCatalog reads OpenAI's Codex catalog body ({"models":[...]}), keeping
// the entries that name a model.
func ParseCatalog(body []byte) ([]map[string]any, error) {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	// Numbers decode as float64, as the client catalog's templates do
	// (registry), so the catalog builder reads both alike.
	if err := json.Unmarshal(bytes.TrimSpace(body), &payload); err != nil {
		return nil, fmt.Errorf("codex account models: the catalog is not JSON: %w", err)
	}
	if payload.Models == nil {
		return nil, errors.New("codex account models: the catalog has no models array")
	}
	entries := make([]map[string]any, 0, len(payload.Models))
	seen := make(map[string]struct{}, len(payload.Models))
	for _, entry := range payload.Models {
		slug := entrySlug(entry)
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		entries = append(entries, entry)
	}
	return entries, nil
}

// IsCompleteClientEntry reports whether entry has every field the Codex client
// catalog requires of a template (registry.ValidateCodexClientModel).
func IsCompleteClientEntry(entry map[string]any) bool {
	return registry.ValidateCodexClientModel(entry) == nil
}

// ModelInfos returns the registry's description of each learned model: the
// catalog's own when it knows the model, else one built from OpenAI's entry.
func ModelInfos(learned Learned) []*registry.ModelInfo {
	out := make([]*registry.ModelInfo, 0, len(learned.Entries))
	for _, entry := range learned.Entries {
		if info := ModelInfo(entry); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// ModelInfo describes one learned model; nil for an entry without a slug.
func ModelInfo(entry map[string]any) *registry.ModelInfo {
	slug := entrySlug(entry)
	if slug == "" {
		return nil
	}
	if known := registry.LookupStaticModelInfoByChannel(slug, "codex"); known != nil {
		return known
	}
	info := &registry.ModelInfo{
		ID:                        slug,
		Object:                    "model",
		OwnedBy:                   "openai",
		Type:                      "openai",
		DisplayName:               entryString(entry, "display_name"),
		Description:               entryString(entry, "description"),
		ContextLength:             entryInt(entry, "context_window"),
		SupportedParameters:       []string{"tools"},
		SupportedInputModalities:  entryStrings(entry, "input_modalities"),
		SupportedOutputModalities: []string{"text"},
	}
	if info.DisplayName == "" {
		info.DisplayName = slug
	}
	if len(info.SupportedInputModalities) == 0 {
		info.SupportedInputModalities = []string{"text"}
	}
	if levels := entryReasoningLevels(entry); len(levels) > 0 {
		info.Thinking = &registry.ThinkingSupport{Levels: levels}
	}
	return info
}

func entryReasoningLevels(entry map[string]any) []string {
	raw, _ := entry["supported_reasoning_levels"].([]any)
	levels := make([]string, 0, len(raw))
	for _, item := range raw {
		level := ""
		switch value := item.(type) {
		case map[string]any:
			level, _ = value["effort"].(string)
		case string:
			level = value
		}
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" || level == orchestrationEffort {
			continue
		}
		levels = append(levels, level)
	}
	return levels
}

func entrySlug(entry map[string]any) string {
	return entryString(entry, "slug")
}

func entryString(entry map[string]any, key string) string {
	if entry == nil {
		return ""
	}
	value, _ := entry[key].(string)
	return strings.TrimSpace(value)
}

func entryInt(entry map[string]any, key string) int {
	if entry == nil {
		return 0
	}
	switch value := entry[key].(type) {
	case json.Number:
		n, err := value.Int64()
		if err == nil && n > 0 {
			return int(n)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	case int:
		if value > 0 {
			return value
		}
	}
	return 0
}

func entryStrings(entry map[string]any, key string) []string {
	raw, _ := entry[key].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}

// CompareVersions orders dotted Codex versions ("0.163.0", "0.164.0-alpha.2")
// by their numeric release parts; a prerelease sorts before its release. An
// unparseable version sorts before every parseable one.
func CompareVersions(a, b string) int {
	pa, preA, okA := parseVersion(a)
	pb, preB, okB := parseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int64
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	case preA < preB:
		return -1
	default:
		return 1
	}
}

// ValidVersion reports whether v is a dotted Codex version.
func ValidVersion(v string) bool {
	_, _, ok := parseVersion(v)
	return ok
}

func parseVersion(v string) ([]int64, string, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || len(v) > 64 {
		return nil, "", false
	}
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre = v[i:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return nil, "", false
	}
	out := make([]int64, len(parts))
	for i, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n < 0 {
			return nil, "", false
		}
		out[i] = n
	}
	return out, pre, true
}

// Learner is the service side: it asks OpenAI for its Codex accounts' catalogs.
type Learner interface {
	// Ensure learns the accounts' catalogs for a Codex client of clientVersion
	// before it is answered a model list: it waits, briefly, for an account
	// that has none yet for that version, and refreshes stale ones behind.
	Ensure(ctx context.Context, clientVersion string)
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
func Ensure(ctx context.Context, clientVersion string) {
	if box := currentLearner.Load(); box != nil {
		box.learner.Ensure(ctx, clientVersion)
	}
}

// Resolve asks the installed learner, if any (Learner.Resolve).
func Resolve(ctx context.Context, model string) bool {
	if box := currentLearner.Load(); box != nil {
		return box.learner.Resolve(ctx, model)
	}
	return false
}
