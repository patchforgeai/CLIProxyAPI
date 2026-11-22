// Package antigravityaccountmodels holds the models Google offers each
// Antigravity account, and the Gemini API names the router serves them under
// (devyre).
//
// Upstream learns which models an Antigravity credential may use only from the
// model catalog (models.json): embedded in the binary, and replaced every three
// hours by a copy downloaded from a third party's repository. Its antigravity
// section names Google's internal ids (gemini-4-neon-xhigh, gemini-3.8-flash-high,
// gemini-3.8-flash-tiered), while the coding agents that reach these accounts
// through the router speak the public Gemini API and ask for its names: Google
// Antigravity CLI 1.5.14 sends gemini-4-neon, gemini-4-carbon or
// gemini-3.8-flash with the chosen level in
// generationConfig.thinkingConfig.thinkingLevel, and Gemini CLI 0.70.0 sends
// whatever id it is given. A static alias table bridged the two, and broke each
// time Google added or renamed a model, and a model the catalog had not caught
// up with was refused outright.
//
// Google itself tells each account what it may use: POST
// <base>/v1internal:fetchAvailableModels, the call upstream already makes for
// its web-search hints, answers the account's models (each with its name,
// limits and whether it is disabled), its model families (grouped_models: a
// family key such as gemini-4-neon whose variants are the key plus one
// thinking suffix per level, with a default level), and the models it retired
// with the model that replaces each. This package keeps that answer per
// credential, describes the models it lists for the registry, and works out the
// Gemini API names an account serves beside its ids (Routes): a family under its
// key, an id with a level suffix under the name without it, and a retired model
// under its own name, served by its replacement. The service fills it and
// registers what it serves (sdk/cliproxy/devyre_antigravity_account_models.go);
// the Antigravity executor sends each name's request to the id it resolves to
// (internal/runtime/executor/devyre_antigravity_account_models.go); the request
// paths reach the service through the Learner hook, since they cannot import it.
package antigravityaccountmodels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// skippedIDs are the ids upstream's catalog generator leaves out
// (cmd/fetch_antigravity_models): internal and experimental models, and ones
// Antigravity does not serve to the API.
var skippedIDs = map[string]struct{}{
	"chat_20706":                  {},
	"chat_23310":                  {},
	"tab_flash_lite_preview":      {},
	"tab_jump_flash_lite_preview": {},
	"gemini-2.5-flash-thinking":   {},
	"gemini-2.5-pro":              {},
}

// levelSuffixes are the thinking levels an id may end in, in order.
var levelSuffixes = []string{"minimal", "low", "medium", "high", "xhigh"}

// googleProvider is ModelDetails.model_provider for Google's own models.
const googleProvider = "MODEL_PROVIDER_GOOGLE"

// Model is one model of Google's answer (ModelDetails), keeping what the router
// serves and describes it by.
type Model struct {
	ID                string
	DisplayName       string
	Description       string
	SupportsImages    bool
	SupportsVideo     bool
	SupportsThinking  bool
	ThinkingBudget    int
	MinThinkingBudget int
	MaxTokens         int
	MaxOutputTokens   int
	Disabled          bool
	Internal          bool
	// Provider is model_provider: MODEL_PROVIDER_GOOGLE, _ANTHROPIC, _OPENAI.
	Provider string
}

// Family is one of Google's grouped models: one model's variants, one per
// thinking level, each the family's key plus the level's suffix.
type Family struct {
	Key         string
	DisplayName string
	// Suffixes are the variants' thinking suffixes ("-low"), in Google's order.
	Suffixes []string
	// DefaultSuffix is the variant Google's own client picks without a level.
	DefaultSuffix string
}

// Answer is what Google answered for one account, as the router uses it.
type Answer struct {
	// Models are sorted by id.
	Models []Model
	// Families are sorted by key.
	Families []Family
	// Deprecated maps each retired id to the id of the model that replaces it.
	Deprecated map[string]string
	// WebSearch are the ids that can run Google Search, lower case and sorted.
	WebSearch []string
}

// Learned is what Google answered for one account.
type Learned struct {
	// At is when it was answered.
	At time.Time
	// Credential identifies the access token it was asked with: a digest,
	// never the token.
	Credential string
	Answer     Answer
}

// Served returns the models an account serves once Google has answered: the
// ones listed that are neither disabled nor internal, nor retired (a model
// deprecatedModelIds names is served by its replacement, under its own name),
// without the ids upstream's catalog generator skips. A partner's model
// (Anthropic's, OpenAI's) is served only under an id the Antigravity catalog
// already has, and never under one another provider's catalog has: an id such
// as claude-sonnet-5-5 belongs to the Claude accounts, whose clients would
// otherwise be sent to Google.
func (a Answer) Served() []Model {
	retired := make(map[string]struct{}, len(a.Deprecated))
	for old := range a.Deprecated {
		retired[strings.ToLower(old)] = struct{}{}
	}
	out := make([]Model, 0, len(a.Models))
	for _, model := range a.Models {
		if model.Disabled || model.Internal {
			continue
		}
		key := strings.ToLower(model.ID)
		if _, skip := skippedIDs[key]; skip {
			continue
		}
		if _, old := retired[key]; old {
			continue
		}
		if !model.Google() && !partnerServed(model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// ServedIDs returns the ids of Served, in order.
func (a Answer) ServedIDs() []string {
	served := a.Served()
	out := make([]string, 0, len(served))
	for _, model := range served {
		out = append(out, model.ID)
	}
	return out
}

// Google reports whether the model is Google's own: by model_provider, else
// by a gemini id.
func (m Model) Google() bool {
	if m.Provider == "" {
		return strings.HasPrefix(strings.ToLower(m.ID), "gemini")
	}
	return strings.EqualFold(m.Provider, googleProvider)
}

// otherCatalogs are the catalogs of the providers whose ids a partner's model
// served through Antigravity must not take.
var otherCatalogs = []string{"claude", "codex", "kimi", "xai", "devin", "meta"}

// partnerServed reports whether a partner's model may be served under its id:
// the Antigravity catalog has the id, and no other provider's catalog does.
func partnerServed(id string) bool {
	if !catalogHas(id, "antigravity") {
		return false
	}
	for _, channel := range otherCatalogs {
		if catalogHas(id, channel) {
			return false
		}
	}
	return true
}

// catalogHas reports whether a provider's catalog lists id (tests replace it).
var catalogHas = func(id, channel string) bool {
	return registry.LookupStaticModelInfoByChannel(id, channel) != nil
}

// wireInt reads an integer that proto3 JSON writes as a number or, for 64-bit
// fields, as a string.
type wireInt int

func (w *wireInt) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		*w = 0
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*w = wireInt(value)
	return nil
}

// wireString reads an enum that proto3 JSON writes as its name, or as its
// number when the name is unknown to the writer.
type wireString string

func (w *wireString) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*w = wireString(text)
		return nil
	}
	*w = wireString(strings.TrimSpace(string(data)))
	return nil
}

type wireModel struct {
	DisplayName       string     `json:"displayName"`
	Description       string     `json:"description"`
	SupportsImages    bool       `json:"supportsImages"`
	SupportsVideo     bool       `json:"supportsVideo"`
	SupportsThinking  bool       `json:"supportsThinking"`
	ThinkingBudget    wireInt    `json:"thinkingBudget"`
	MinThinkingBudget wireInt    `json:"minThinkingBudget"`
	MaxTokens         wireInt    `json:"maxTokens"`
	MaxOutputTokens   wireInt    `json:"maxOutputTokens"`
	Disabled          bool       `json:"disabled"`
	IsInternal        bool       `json:"isInternal"`
	ModelProvider     wireString `json:"modelProvider"`
}

type wireAnswer struct {
	Models             map[string]wireModel `json:"models"`
	WebSearchModelIDs  []string             `json:"webSearchModelIds"`
	DeprecatedModelIDs map[string]struct {
		NewModelID string `json:"newModelId"`
	} `json:"deprecatedModelIds"`
	GroupedModels map[string]struct {
		DisplayName             string `json:"displayName"`
		SupportedThinkingLevels []struct {
			ThinkingSuffix string `json:"thinkingSuffix"`
		} `json:"supportedThinkingLevels"`
		DefaultThinkingSuffix string `json:"defaultThinkingSuffix"`
	} `json:"groupedModels"`
}

// Parse reads Google's fetchAvailableModels answer
// (FetchAvailableModelsResponse in proto3 JSON).
func Parse(body []byte) (Answer, error) {
	var wire wireAnswer
	if err := json.Unmarshal(bytes.TrimSpace(body), &wire); err != nil {
		return Answer{}, fmt.Errorf("antigravity account models: the answer is not JSON: %w", err)
	}
	if wire.Models == nil {
		return Answer{}, errors.New("antigravity account models: the answer has no models")
	}
	var answer Answer
	for id, model := range wire.Models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		answer.Models = append(answer.Models, Model{
			ID:                id,
			DisplayName:       strings.TrimSpace(model.DisplayName),
			Description:       strings.TrimSpace(model.Description),
			SupportsImages:    model.SupportsImages,
			SupportsVideo:     model.SupportsVideo,
			SupportsThinking:  model.SupportsThinking,
			ThinkingBudget:    int(model.ThinkingBudget),
			MinThinkingBudget: int(model.MinThinkingBudget),
			MaxTokens:         int(model.MaxTokens),
			MaxOutputTokens:   int(model.MaxOutputTokens),
			Disabled:          model.Disabled,
			Internal:          model.IsInternal,
			Provider:          strings.TrimSpace(string(model.ModelProvider)),
		})
	}
	sort.Slice(answer.Models, func(i, j int) bool { return answer.Models[i].ID < answer.Models[j].ID })
	for key, group := range wire.GroupedModels {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		family := Family{Key: key, DisplayName: strings.TrimSpace(group.DisplayName), DefaultSuffix: strings.TrimSpace(group.DefaultThinkingSuffix)}
		for _, level := range group.SupportedThinkingLevels {
			if suffix := strings.TrimSpace(level.ThinkingSuffix); suffix != "" {
				family.Suffixes = append(family.Suffixes, suffix)
			}
		}
		answer.Families = append(answer.Families, family)
	}
	sort.Slice(answer.Families, func(i, j int) bool { return answer.Families[i].Key < answer.Families[j].Key })
	for old, info := range wire.DeprecatedModelIDs {
		old, replacement := strings.TrimSpace(old), strings.TrimSpace(info.NewModelID)
		if old == "" || replacement == "" {
			continue
		}
		if answer.Deprecated == nil {
			answer.Deprecated = make(map[string]string)
		}
		answer.Deprecated[old] = replacement
	}
	for _, id := range wire.WebSearchModelIDs {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			answer.WebSearch = append(answer.WebSearch, id)
		}
	}
	sort.Strings(answer.WebSearch)
	return answer, nil
}

// LevelName is a thinking suffix's level: without its dash, in lower case.
func LevelName(suffix string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(suffix), "-"))
}

// variantID is the id of the family's variant for suffix: the key and the
// suffix, read with a dash when the suffix has none and only that form is a
// model Google lists.
func (f Family) variantID(suffix string, listed map[string]struct{}) string {
	id := f.Key + suffix
	if suffix == "" || strings.HasPrefix(suffix, "-") {
		return id
	}
	if _, ok := listed[strings.ToLower(id)]; ok {
		return id
	}
	if _, ok := listed[strings.ToLower(f.Key+"-"+suffix)]; ok {
		return f.Key + "-" + suffix
	}
	return id
}

// levelRank orders the thinking levels.
var levelRank = map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// levelInName is a display name's trailing level, as Google writes it
// ("Gemini 4 Neon (Low)", "(XHigh)", "(Extra High)").
var levelInName = regexp.MustCompile(`(?i)\s*\((?:minimal|low|medium|high|x-?high|extra[ -]?high|max)\)\s*$`)

// withoutLevel is a display name without its trailing level.
func withoutLevel(name string) string {
	return strings.TrimSpace(levelInName.ReplaceAllString(name, ""))
}

// Route is a Gemini API name an account serves beside its ids, and the served
// ids a request for it reaches: one per thinking level, and one for a request
// that names none.
type Route struct {
	Name        string
	DisplayName string
	// Levels are the route's thinking levels, lower case, in order.
	Levels []string
	// ByLevel maps each level to the id that serves it.
	ByLevel map[string]string
	// Default serves a request that names no level: Google's default variant
	// when the account serves it, else the high one, else the highest.
	Default string
}

// Resolve returns the served id a request for the route at level reaches: the
// level's own id; for a level the route lacks, the nearest one it has (a tie
// to the higher, so xhigh or max above its top reach its highest); without a
// level ("", auto, none, or one no level order knows), the default.
func (r Route) Resolve(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if id, ok := r.ByLevel[level]; ok && id != "" {
		return id
	}
	want, known := levelRank[level]
	if !known {
		return r.Default
	}
	best, bestRank, found := "", 0, false
	for _, served := range r.Levels {
		rank, ok := levelRank[served]
		id := r.ByLevel[served]
		if !ok || id == "" {
			continue
		}
		distance, bestDistance := abs(rank-want), abs(bestRank-want)
		if !found || distance < bestDistance || (distance == bestDistance && rank > bestRank) {
			best, bestRank, found = id, rank, true
		}
	}
	if !found {
		return r.Default
	}
	return best
}

// LevelOf returns the thinking level the variant id serves: the level the
// route reaches it by, else the level suffix the id ends in; "" when neither.
func (r Route) LevelOf(id string) string {
	for _, level := range r.Levels {
		if strings.EqualFold(r.ByLevel[level], id) {
			return level
		}
	}
	_, level := splitLevel(id)
	return level
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// defaultID is the id a route reaches without a level: Google's default
// variant when served (googleDefault), else the high one, else the highest.
func defaultID(byLevel map[string]string, levels []string, googleDefault string) string {
	if googleDefault != "" {
		return googleDefault
	}
	if id, ok := byLevel["high"]; ok {
		return id
	}
	best, bestRank := "", -1
	for _, level := range levels {
		if rank, ok := levelRank[level]; ok && rank > bestRank {
			best, bestRank = byLevel[level], rank
		}
	}
	if best != "" {
		return best
	}
	return byLevel[levels[len(levels)-1]]
}

// Routes returns the Gemini API names an account serves beside its ids, given
// the ids it serves (Served, less what the configuration excludes):
//
//   - a family whose variants it serves, under the family's key (unless the key
//     is itself a served id), with the served variants' levels;
//   - an id in no family that ends in a level suffix, under the name without
//     it, when that name is neither served nor a family's key; ids sharing the
//     name become its levels;
//   - a retired id whose replacement it serves, under the retired id, served
//     as the replacement is.
//
// A name's display name is its family's, else its default id's without the
// trailing level. Only Google's own models get such names: an Anthropic or
// OpenAI model keeps its id, since a name like claude-opus-5-5 belongs to the
// Claude accounts.
func Routes(answer Answer, served []string) []Route {
	models := make(map[string]Model, len(answer.Models))
	listed := make(map[string]struct{}, len(answer.Models))
	for _, model := range answer.Models {
		key := strings.ToLower(model.ID)
		models[key] = model
		listed[key] = struct{}{}
	}
	serves := make(map[string]string, len(served))
	for _, id := range served {
		if id = strings.TrimSpace(id); id != "" {
			serves[strings.ToLower(id)] = id
		}
	}
	google := func(id string) bool {
		model, ok := models[strings.ToLower(id)]
		return ok && model.Google()
	}

	routes := make(map[string]*Route)
	var order []string
	add := func(route *Route) {
		key := strings.ToLower(route.Name)
		routes[key] = route
		order = append(order, key)
	}

	familyKeys := make(map[string]struct{}, len(answer.Families))
	variants := make(map[string]struct{})
	for _, family := range answer.Families {
		familyKeys[strings.ToLower(family.Key)] = struct{}{}
		for _, suffix := range family.Suffixes {
			variants[strings.ToLower(family.variantID(suffix, listed))] = struct{}{}
		}
	}

	for _, family := range answer.Families {
		if _, isID := serves[strings.ToLower(family.Key)]; isID {
			continue
		}
		route := &Route{Name: family.Key, ByLevel: make(map[string]string)}
		for _, suffix := range family.Suffixes {
			id, ok := serves[strings.ToLower(family.variantID(suffix, listed))]
			level := LevelName(suffix)
			if !ok || !google(id) || level == "" {
				continue
			}
			if _, dup := route.ByLevel[level]; dup {
				continue
			}
			route.ByLevel[level] = id
			route.Levels = append(route.Levels, level)
		}
		if len(route.Levels) == 0 {
			continue
		}
		googleDefault := ""
		if family.DefaultSuffix != "" {
			if id, ok := serves[strings.ToLower(family.variantID(family.DefaultSuffix, listed))]; ok && google(id) {
				googleDefault = id
			}
		}
		route.Default = defaultID(route.ByLevel, route.Levels, googleDefault)
		route.DisplayName = family.DisplayName
		if route.DisplayName == "" {
			route.DisplayName = withoutLevel(displayName(models, route.Default))
		}
		add(route)
	}

	ids := make([]string, 0, len(serves))
	for _, id := range serves {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stripped := make(map[string]*Route)
	for _, id := range ids {
		if _, inFamily := variants[strings.ToLower(id)]; inFamily || !google(id) {
			continue
		}
		base, level := splitLevel(id)
		if base == "" {
			continue
		}
		key := strings.ToLower(base)
		if _, isID := serves[key]; isID {
			continue
		}
		if _, isFamily := familyKeys[key]; isFamily {
			continue
		}
		route, ok := stripped[key]
		if !ok {
			route = &Route{Name: base, ByLevel: make(map[string]string)}
			stripped[key] = route
			add(route)
		}
		route.ByLevel[level] = id
	}
	for _, route := range stripped {
		for _, level := range levelSuffixes {
			if _, ok := route.ByLevel[level]; ok {
				route.Levels = append(route.Levels, level)
			}
		}
		route.Default = defaultID(route.ByLevel, route.Levels, "")
		route.DisplayName = withoutLevel(displayName(models, route.Default))
	}

	retired := make([]string, 0, len(answer.Deprecated))
	for old := range answer.Deprecated {
		retired = append(retired, old)
	}
	sort.Strings(retired)
	for _, old := range retired {
		key := strings.ToLower(old)
		if _, isID := serves[key]; isID {
			continue
		}
		if _, named := routes[key]; named {
			continue
		}
		replacement := answer.Deprecated[old]
		if id, ok := serves[strings.ToLower(replacement)]; ok {
			if !google(id) {
				continue
			}
			add(&Route{Name: old, DisplayName: withoutLevel(displayName(models, id)), Default: id, ByLevel: map[string]string{}})
			continue
		}
		target, ok := routes[strings.ToLower(replacement)]
		if !ok {
			continue
		}
		route := &Route{Name: old, DisplayName: target.DisplayName, Levels: append([]string(nil), target.Levels...), ByLevel: make(map[string]string, len(target.ByLevel)), Default: target.Default}
		for level, id := range target.ByLevel {
			route.ByLevel[level] = id
		}
		add(route)
	}

	out := make([]Route, 0, len(order))
	for _, key := range order {
		out = append(out, *routes[key])
	}
	return out
}

// splitLevel splits an id ending in a level suffix into the name before it and
// the level; base is empty when the id has no such suffix.
func splitLevel(id string) (base, level string) {
	lower := strings.ToLower(id)
	for _, candidate := range levelSuffixes {
		suffix := "-" + candidate
		if strings.HasSuffix(lower, suffix) && len(id) > len(suffix) {
			return id[:len(id)-len(suffix)], candidate
		}
	}
	return "", ""
}

func displayName(models map[string]Model, id string) string {
	if model, ok := models[strings.ToLower(id)]; ok && model.DisplayName != "" {
		return model.DisplayName
	}
	return id
}

// ModelInfos returns the registry's description of each model the account
// serves (Served): the catalog's own when it knows the id, else one built from
// Google's entry. A family's variant takes the family's thinking levels, and a
// model Google lists for Google Search is marked for it.
func ModelInfos(answer Answer) []*registry.ModelInfo {
	served := answer.Served()
	levels := familyLevels(answer)
	webSearch := make(map[string]struct{}, len(answer.WebSearch))
	for _, id := range answer.WebSearch {
		webSearch[id] = struct{}{}
	}
	out := make([]*registry.ModelInfo, 0, len(served))
	for _, model := range served {
		key := strings.ToLower(model.ID)
		_, search := webSearch[key]
		if info := ModelInfo(model, levels[key], search); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// familyLevels maps each family variant's id (lower case) to its family's
// levels.
func familyLevels(answer Answer) map[string][]string {
	listed := make(map[string]struct{}, len(answer.Models))
	for _, model := range answer.Models {
		listed[strings.ToLower(model.ID)] = struct{}{}
	}
	out := make(map[string][]string)
	for _, family := range answer.Families {
		var levels []string
		for _, suffix := range family.Suffixes {
			if level := LevelName(suffix); level != "" {
				levels = append(levels, level)
			}
		}
		for _, suffix := range family.Suffixes {
			out[strings.ToLower(family.variantID(suffix, listed))] = levels
		}
	}
	return out
}

// ModelInfo describes one served model: the catalog's entry when the catalog
// knows the id, else Google's (its name, max_tokens as the context window, its
// output limit, image and video input, thinking). levels, when given, are the
// model's family's thinking levels.
func ModelInfo(model Model, levels []string, webSearch bool) *registry.ModelInfo {
	if model.ID == "" {
		return nil
	}
	info := registry.LookupStaticModelInfoByChannel(model.ID, "antigravity")
	if info == nil {
		info = &registry.ModelInfo{
			ID:                        model.ID,
			Object:                    "model",
			OwnedBy:                   "antigravity",
			Type:                      "antigravity",
			DisplayName:               model.DisplayName,
			Name:                      model.ID,
			Description:               model.Description,
			ContextLength:             model.MaxTokens,
			MaxCompletionTokens:       model.MaxOutputTokens,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		}
		if info.DisplayName == "" {
			info.DisplayName = model.ID
		}
		if info.Description == "" {
			info.Description = info.DisplayName
		}
		if model.SupportsImages {
			info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
		}
		if model.SupportsVideo {
			info.SupportedInputModalities = append(info.SupportedInputModalities, "video")
		}
		if model.SupportsThinking || len(levels) > 0 {
			info.Thinking = &registry.ThinkingSupport{DynamicAllowed: true}
			if model.ThinkingBudget > 0 {
				info.Thinking.Min = max(model.MinThinkingBudget, 0)
				info.Thinking.Max = model.ThinkingBudget
			}
		}
	}
	if len(levels) > 0 {
		if info.Thinking == nil {
			info.Thinking = &registry.ThinkingSupport{DynamicAllowed: true}
		}
		info.Thinking.Levels = append([]string(nil), levels...)
	}
	if webSearch {
		info.SupportsWebSearch = true
	}
	return info
}

// RouteInfo describes a route for the registry: its default id's description
// under the route's name, display name and levels. infos are the served ids'
// descriptions by lower-case id; nil when the default is not among them.
func RouteInfo(route Route, infos map[string]*registry.ModelInfo) *registry.ModelInfo {
	target, ok := infos[strings.ToLower(route.Default)]
	if !ok || target == nil {
		return nil
	}
	info := *target
	info.ID = route.Name
	info.Name = route.Name
	info.MetadataModelID = target.ID
	if route.DisplayName != "" {
		info.DisplayName = route.DisplayName
		info.Description = route.DisplayName
	}
	if target.Thinking != nil {
		thinking := *target.Thinking
		info.Thinking = &thinking
	}
	if target.NativeCapabilities != nil {
		capabilities := *target.NativeCapabilities
		info.NativeCapabilities = &capabilities
	}
	if len(route.Levels) > 0 {
		if info.Thinking == nil {
			info.Thinking = &registry.ThinkingSupport{DynamicAllowed: true}
		}
		info.Thinking.Levels = append([]string(nil), route.Levels...)
	}
	info.SupportedInputModalities = append([]string(nil), target.SupportedInputModalities...)
	info.SupportedOutputModalities = append([]string(nil), target.SupportedOutputModalities...)
	return &info
}

// Store keeps, by credential ID, Google's answer and the routes registered
// from it.
type Store struct {
	mu     sync.RWMutex
	byAuth map[string]Learned
	routes map[string]map[string]Route
}

var defaultStore = NewStore()

// Default returns the store the service fills and the registration and the
// executor read.
func Default() *Store { return defaultStore }

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{byAuth: make(map[string]Learned), routes: make(map[string]map[string]Route)}
}

// Set records what Google answered for authID and reports whether the answer
// differs from what was recorded before.
func (s *Store) Set(authID string, learned Learned) bool {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, had := s.byAuth[authID]
	s.byAuth[authID] = learned
	return !had || !reflect.DeepEqual(previous.Answer, learned.Answer)
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

// SetRoutes records the routes registered for authID; none removes them.
func (s *Store) SetRoutes(authID string, routes []Route) {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(routes) == 0 {
		delete(s.routes, authID)
		return
	}
	byName := make(map[string]Route, len(routes))
	for _, route := range routes {
		byName[strings.ToLower(route.Name)] = route
	}
	s.routes[authID] = byName
}

// Route returns the route registered for authID under name.
func (s *Store) Route(authID, name string) (Route, bool) {
	if s == nil {
		return Route{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	route, ok := s.routes[strings.TrimSpace(authID)][strings.ToLower(strings.TrimSpace(name))]
	return route, ok
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
	for id := range s.routes {
		if _, ok := keep[id]; !ok {
			delete(s.routes, id)
		}
	}
}

// Learner is the service side: it asks Google for its Antigravity accounts'
// models.
type Learner interface {
	// Ensure learns the accounts' models before a Gemini client is answered a
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
