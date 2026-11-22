package antigravityaccountmodels

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func readFixture(t *testing.T, name string) Answer {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	answer, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return answer
}

func routesByName(routes []Route) map[string]Route {
	out := make(map[string]Route, len(routes))
	for _, route := range routes {
		out[route.Name] = route
	}
	return out
}

func TestParse_ReadsGooglesAnswer(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	if len(answer.Models) != 23 {
		t.Fatalf("models = %d, want every model listed", len(answer.Models))
	}
	byID := make(map[string]Model)
	for _, model := range answer.Models {
		byID[model.ID] = model
	}
	neon := byID["gemini-4-neon-xhigh"]
	if neon.DisplayName != "Gemini 4 Neon (XHigh)" || neon.MaxTokens != 1048576 || neon.MaxOutputTokens != 65536 || !neon.SupportsThinking || neon.Provider != "MODEL_PROVIDER_GOOGLE" {
		t.Fatalf("neon = %+v (64-bit counts arrive as strings)", neon)
	}
	if !byID["gemini-3.6-flash-high"].Disabled || !byID["gemini-4-argon-preview"].Internal {
		t.Fatal("disabled and internal flags are read")
	}
	if len(answer.Families) != 4 || answer.Families[2].Key != "gemini-4-carbon" || strings.Join(answer.Families[3].Suffixes, ",") != "-low,-medium,-high,-xhigh" || answer.Families[3].DefaultSuffix != "-xhigh" {
		t.Fatalf("families = %+v", answer.Families)
	}
	if answer.Deprecated["gemini-3.7-flash"] != "gemini-3.8-flash" || answer.Deprecated["gemini-3.7-flash-high"] != "gemini-3.8-flash-high" || answer.Deprecated["gemini-3.7-flash-low"] != "gemini-3.8-flash-low" {
		t.Fatalf("deprecated = %v", answer.Deprecated)
	}
	if strings.Join(answer.WebSearch, ",") != "gemini-3.5-flash-lite,gemini-3.8-flash-high,gemini-4-carbon-high,gemini-4-neon-high,gemini-4-neon-low,gemini-4-neon-medium,gemini-4-neon-xhigh" {
		t.Fatalf("web search = %v", answer.WebSearch)
	}
}

func TestParse_Refusals(t *testing.T) {
	for _, body := range []string{``, `not json`, `{}`, `{"webSearchModelIds":[]}`} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("Parse(%q) took an answer without models", body)
		}
	}
	answer, err := Parse([]byte(`{"models":{}}`))
	if err != nil || len(answer.Served()) != 0 {
		t.Fatalf("an empty list is an answer that serves nothing: %+v, %v", answer, err)
	}
}

// What an account serves: no disabled, internal, skipped or retired id, and a
// partner's model only under an id the Antigravity catalog has and no other
// provider's catalog has.
func TestServed(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	if got := strings.Join(answer.ServedIDs(), ","); got != "claude-opus-5-5-high,gemini-3.1-pro-low,gemini-3.5-flash-lite,gemini-3.8-flash-high,gemini-3.8-flash-low,gemini-3.8-flash-medium,gemini-3.8-flash-tiered,gemini-4-carbon-high,gemini-4-carbon-low,gemini-4-carbon-medium,gemini-4-neon-high,gemini-4-neon-low,gemini-4-neon-medium,gemini-4-neon-xhigh,gemini-pro-agent,gpt-oss-120b-medium" {
		t.Fatalf("served = %s", got)
	}
	if got := strings.Join(readFixture(t, "fetch_available_models_free.json").ServedIDs(), ","); got != "gemini-3.5-flash-lite,gemini-3.8-flash-high,gemini-3.8-flash-low,gemini-3.8-flash-medium,gemini-3.8-flash-tiered,gemini-4-carbon-high,gemini-4-carbon-low,gemini-4-carbon-medium" {
		t.Fatalf("free serves %s: claude-sonnet-5-5 is the Claude accounts' id", got)
	}

	// The other providers' catalogs are asked too.
	original := catalogHas
	t.Cleanup(func() { catalogHas = original })
	catalogHas = func(id, channel string) bool {
		return (channel == "antigravity" && (id == "claude-shared" || id == "claude-own")) || (channel == "claude" && id == "claude-shared")
	}
	partners := Answer{Models: []Model{
		{ID: "claude-shared", Provider: "MODEL_PROVIDER_ANTHROPIC"},
		{ID: "claude-own", Provider: "MODEL_PROVIDER_ANTHROPIC"},
		{ID: "claude-unknown", Provider: "MODEL_PROVIDER_ANTHROPIC"},
		{ID: "gemini-unlisted-by-the-catalog"},
	}}
	if got := strings.Join(partners.ServedIDs(), ","); got != "claude-own,gemini-unlisted-by-the-catalog" {
		t.Fatalf("served = %s", got)
	}
}

// The names the full account serves beside its ids, and what each reaches.
func TestRoutes_FullAccount(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	routes := Routes(answer, answer.ServedIDs())
	var names []string
	for _, route := range routes {
		names = append(names, route.Name)
	}
	if got := strings.Join(names, ","); got != "gemini-3.8-flash,gemini-4-carbon,gemini-4-neon,gemini-3.1-pro,gemini-3.7-flash,gemini-3.7-flash-high,gemini-3.7-flash-low" {
		t.Fatalf("routes = %s", got)
	}
	byName := routesByName(routes)
	for _, tc := range []struct{ name, level, want string }{
		{"gemini-4-neon", "LOW", "gemini-4-neon-low"},
		{"gemini-4-neon", "medium", "gemini-4-neon-medium"},
		{"gemini-4-neon", "high", "gemini-4-neon-high"},
		{"gemini-4-neon", "xhigh", "gemini-4-neon-xhigh"},
		{"gemini-4-neon", "", "gemini-4-neon-xhigh"},
		{"gemini-4-neon", "auto", "gemini-4-neon-xhigh"},
		{"gemini-4-neon", "max", "gemini-4-neon-xhigh"},
		{"gemini-4-neon", "minimal", "gemini-4-neon-low"},
		{"gemini-4-carbon", "low", "gemini-4-carbon-low"},
		{"gemini-4-carbon", "xhigh", "gemini-4-carbon-high"},
		{"gemini-4-carbon", "max", "gemini-4-carbon-high"},
		{"gemini-4-carbon", "", "gemini-4-carbon-high"},
		{"gemini-3.8-flash", "", "gemini-3.8-flash-high"},
		{"gemini-3.8-flash", "medium", "gemini-3.8-flash-medium"},
		{"gemini-3.8-flash", "minimal", "gemini-3.8-flash-low"},
		{"gemini-3.1-pro", "", "gemini-3.1-pro-low"},
		{"gemini-3.1-pro", "high", "gemini-3.1-pro-low"},
		{"gemini-3.7-flash", "", "gemini-3.8-flash-high"},
		{"gemini-3.7-flash", "LOW", "gemini-3.8-flash-low"},
		{"gemini-3.7-flash-high", "low", "gemini-3.8-flash-high"},
		{"gemini-3.7-flash-low", "", "gemini-3.8-flash-low"},
	} {
		route, ok := byName[tc.name]
		if !ok {
			t.Fatalf("%s is not served", tc.name)
		}
		if got := route.Resolve(tc.level); got != tc.want {
			t.Fatalf("%s at %q = %s, want %s", tc.name, tc.level, got, tc.want)
		}
	}
	if neon := byName["gemini-4-neon"]; strings.Join(neon.Levels, ",") != "low,medium,high,xhigh" {
		t.Fatalf("neon = %+v", neon)
	}
	for _, name := range []string{"gemini-4-argon", "gemini-3.6-flash", "claude-opus-5-5", "gpt-oss-120b", "gemini-3.8-flash-tiered", "gemini-pro-agent"} {
		if _, ok := byName[name]; ok {
			t.Fatalf("%s is served under a name it must not have", name)
		}
	}
}

// A name's display name is its family's, else its default id's without the
// level Google writes after it.
func TestRoutes_DisplayNames(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	byName := routesByName(Routes(answer, answer.ServedIDs()))
	for name, want := range map[string]string{
		"gemini-4-neon":         "Gemini 4 Neon",
		"gemini-4-carbon":       "Gemini 4 Carbon",
		"gemini-3.8-flash":      "Gemini 3.8 Flash",
		"gemini-3.1-pro":        "Gemini 3.1 Pro",
		"gemini-3.7-flash":      "Gemini 3.8 Flash",
		"gemini-3.7-flash-high": "Gemini 3.8 Flash",
		"gemini-3.7-flash-low":  "Gemini 3.8 Flash",
	} {
		if got := byName[name].DisplayName; got != want {
			t.Fatalf("%s display name = %q, want %q", name, got, want)
		}
	}
	// A family Google gives no display name takes its default variant's.
	for i := range answer.Families {
		answer.Families[i].DisplayName = ""
	}
	byName = routesByName(Routes(answer, answer.ServedIDs()))
	if got := byName["gemini-4-neon"].DisplayName; got != "Gemini 4 Neon" {
		t.Fatalf("neon display name = %q", got)
	}
	for in, want := range map[string]string{
		"Gemini 4 Neon (Low)":        "Gemini 4 Neon",
		"Gemini 4 Neon (XHigh)":      "Gemini 4 Neon",
		"Gemini 4 Neon (Extra High)": "Gemini 4 Neon",
		"Gemini 3.8 Flash (Tiered)":  "Gemini 3.8 Flash (Tiered)",
		"Gemini 3.8 Flash":           "Gemini 3.8 Flash",
	} {
		if got := withoutLevel(in); got != want {
			t.Fatalf("withoutLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A retired id Google still lists is not served, and makes no name of its
// own: its retired name reaches the replacement.
func TestRoutes_RetiredIDsGoToTheirReplacement(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	for _, id := range answer.ServedIDs() {
		if strings.HasPrefix(id, "gemini-3.7-flash") {
			t.Fatalf("%s is retired", id)
		}
	}
	byName := routesByName(Routes(answer, answer.ServedIDs()))
	if got := byName["gemini-3.7-flash-low"].Resolve("high"); got != "gemini-3.8-flash-low" {
		t.Fatalf("gemini-3.7-flash-low = %s", got)
	}
	if got := byName["gemini-3.7-flash"].Resolve("medium"); got != "gemini-3.8-flash-medium" {
		t.Fatalf("gemini-3.7-flash = %s, want the replacement family", got)
	}
}

// A free account without Neon serves no gemini-4-neon, although Google's
// families still name it.
func TestRoutes_FreeAccount(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_free.json")
	byName := routesByName(Routes(answer, answer.ServedIDs()))
	if _, ok := byName["gemini-4-neon"]; ok {
		t.Fatal("a family the account serves no variant of is not served")
	}
	if len(byName) != 5 || byName["gemini-4-carbon"].Resolve("") != "gemini-4-carbon-high" || byName["gemini-3.7-flash"].Resolve("low") != "gemini-3.8-flash-low" {
		t.Fatalf("routes = %+v", byName)
	}
}

// What the configuration excludes is not served, and the routes follow: a
// level the route lost reaches the nearest one served, and without a level
// the high variant answers when Google's default is gone.
func TestRoutes_FollowTheServedIDs(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	var served []string
	for _, id := range answer.ServedIDs() {
		if id != "gemini-4-neon-xhigh" && id != "gemini-4-neon-low" && !strings.HasPrefix(id, "gemini-4-carbon") {
			served = append(served, id)
		}
	}
	byName := routesByName(Routes(answer, served))
	neon := byName["gemini-4-neon"]
	if strings.Join(neon.Levels, ",") != "medium,high" {
		t.Fatalf("neon = %+v", neon)
	}
	for level, want := range map[string]string{"": "gemini-4-neon-high", "xhigh": "gemini-4-neon-high", "max": "gemini-4-neon-high", "low": "gemini-4-neon-medium", "minimal": "gemini-4-neon-medium", "none": "gemini-4-neon-high"} {
		if got := neon.Resolve(level); got != want {
			t.Fatalf("neon at %q = %s, want %s", level, got, want)
		}
	}
	if _, ok := byName["gemini-4-carbon"]; ok {
		t.Fatal("a family without a served variant is not served")
	}

	// Without Google's default nor a high variant, the highest answers.
	var lowOnly []string
	for _, id := range answer.ServedIDs() {
		if !strings.HasPrefix(id, "gemini-4-neon-") || id == "gemini-4-neon-low" || id == "gemini-4-neon-medium" {
			lowOnly = append(lowOnly, id)
		}
	}
	if got := routesByName(Routes(answer, lowOnly))["gemini-4-neon"].Resolve(""); got != "gemini-4-neon-medium" {
		t.Fatalf("neon without high variants = %s", got)
	}

	// A family key that is itself a served id keeps the id.
	answer.Models = append(answer.Models, Model{ID: "gemini-3.8-flash", Provider: googleProvider})
	if _, ok := routesByName(Routes(answer, append(answer.ServedIDs(), "gemini-3.8-flash")))["gemini-3.8-flash"]; ok {
		t.Fatal("a served id is never a route")
	}
}

func TestRoutes_StrippedNamesShareLevels(t *testing.T) {
	answer := Answer{Models: []Model{
		{ID: "gemini-3.1-pro-low", DisplayName: "Gemini 3.1 Pro (Low)", Provider: googleProvider},
		{ID: "gemini-3.1-pro-high", DisplayName: "Gemini 3.1 Pro (High)", Provider: googleProvider},
		{ID: "gemini-3-flash", Provider: googleProvider},
		{ID: "gemini-3-flash-high", Provider: googleProvider},
	}}
	byName := routesByName(Routes(answer, answer.ServedIDs()))
	pro := byName["gemini-3.1-pro"]
	if strings.Join(pro.Levels, ",") != "low,high" || pro.DisplayName != "Gemini 3.1 Pro" {
		t.Fatalf("pro = %+v", pro)
	}
	for level, want := range map[string]string{"": "gemini-3.1-pro-high", "low": "gemini-3.1-pro-low", "medium": "gemini-3.1-pro-high", "minimal": "gemini-3.1-pro-low", "xhigh": "gemini-3.1-pro-high"} {
		if got := pro.Resolve(level); got != want {
			t.Fatalf("pro at %q = %s, want %s", level, got, want)
		}
	}
	if _, ok := byName["gemini-3-flash"]; ok || len(byName) != 1 {
		t.Fatalf("routes = %+v, want no name shadowing a served id", byName)
	}
}

// A thinking suffix written without its dash still names the variant.
func TestRoutes_SuffixesWithoutADash(t *testing.T) {
	answer := Answer{
		Models: []Model{
			{ID: "gemini-4-argon-high", DisplayName: "Gemini 4 Argon (High)", Provider: googleProvider},
			{ID: "gemini-4-argon-xhigh", DisplayName: "Gemini 4 Argon (XHigh)", Provider: googleProvider},
		},
		Families: []Family{{Key: "gemini-4-argon", Suffixes: []string{"high", "xhigh"}, DefaultSuffix: "xhigh"}},
	}
	routes := Routes(answer, answer.ServedIDs())
	if len(routes) != 1 || routes[0].Name != "gemini-4-argon" || routes[0].Resolve("") != "gemini-4-argon-xhigh" || routes[0].Resolve("low") != "gemini-4-argon-high" || routes[0].DisplayName != "Gemini 4 Argon" {
		t.Fatalf("routes = %+v", routes)
	}
	if levels := familyLevels(answer)["gemini-4-argon-high"]; strings.Join(levels, ",") != "high,xhigh" {
		t.Fatalf("levels = %v", levels)
	}
}

func TestModelInfos(t *testing.T) {
	answer := readFixture(t, "fetch_available_models_full.json")
	infos := ModelInfos(answer)
	byID := make(map[string]*registry.ModelInfo)
	for _, info := range infos {
		byID[info.ID] = info
	}
	if len(infos) != len(answer.Served()) {
		t.Fatalf("infos = %d, want one per served model", len(infos))
	}
	// The catalog knows gemini-3.8-flash-high: its description, with the
	// family's levels.
	known := byID["gemini-3.8-flash-high"]
	catalog := registry.LookupStaticModelInfoByChannel("gemini-3.8-flash-high", "antigravity")
	if catalog == nil {
		t.Fatal("the embedded catalog lists gemini-3.8-flash-high")
	}
	if known.DisplayName != catalog.DisplayName || known.ContextLength != catalog.ContextLength || strings.Join(known.Thinking.Levels, ",") != "low,medium,high" || !known.SupportsWebSearch {
		t.Fatalf("known = %+v", known)
	}
	// Neon is Google's alone.
	neon := byID["gemini-4-neon-low"]
	if neon.DisplayName != "Gemini 4 Neon (Low)" || neon.Name != "gemini-4-neon-low" || neon.ContextLength != 1048576 || neon.MaxCompletionTokens != 65536 || neon.Type != "antigravity" || neon.OwnedBy != "antigravity" {
		t.Fatalf("neon = %+v", neon)
	}
	if neon.Thinking == nil || strings.Join(neon.Thinking.Levels, ",") != "low,medium,high,xhigh" || neon.Thinking.Max != 65535 || neon.Thinking.Min != 1 || !neon.SupportsWebSearch {
		t.Fatalf("neon thinking = %+v web search = %v", neon.Thinking, neon.SupportsWebSearch)
	}
	if strings.Join(neon.SupportedInputModalities, ",") != "text,image,video" {
		t.Fatalf("neon input = %v", neon.SupportedInputModalities)
	}
	// Web search from Google's hints, case aside, for a catalog model that has none.
	if lite := byID["gemini-3.5-flash-lite"]; !lite.SupportsWebSearch {
		t.Fatalf("lite = %+v", lite)
	}
	if carbon := byID["gemini-4-carbon-low"]; carbon.SupportsWebSearch {
		t.Fatal("a model Google does not list for search is not marked")
	}
	if catalogAfter := registry.LookupStaticModelInfoByChannel("gemini-3.8-flash-high", "antigravity"); strings.Join(catalogAfter.Thinking.Levels, ",") != strings.Join(catalog.Thinking.Levels, ",") {
		t.Fatal("the catalog itself is never changed")
	}

	routes := Routes(answer, answer.ServedIDs())
	byName := routesByName(routes)
	infosByID := make(map[string]*registry.ModelInfo)
	for _, info := range infos {
		infosByID[strings.ToLower(info.ID)] = info
	}
	family := RouteInfo(byName["gemini-4-neon"], infosByID)
	if family.ID != "gemini-4-neon" || family.Name != "gemini-4-neon" || family.DisplayName != "Gemini 4 Neon" || family.MetadataModelID != "gemini-4-neon-xhigh" || strings.Join(family.Thinking.Levels, ",") != "low,medium,high,xhigh" {
		t.Fatalf("family = %+v", family)
	}
	retired := RouteInfo(byName["gemini-3.7-flash"], infosByID)
	if retired.ID != "gemini-3.7-flash" || retired.DisplayName != "Gemini 3.8 Flash" || retired.MetadataModelID != "gemini-3.8-flash-high" {
		t.Fatalf("retired = %+v", retired)
	}
	family.Thinking.Levels[0] = "changed"
	if infosByID["gemini-4-neon-xhigh"].Thinking.Levels[0] != "low" {
		t.Fatal("a route's description is its own copy")
	}
}

func TestStore(t *testing.T) {
	store := NewStore()
	answer := readFixture(t, "fetch_available_models_full.json")
	if !store.Set("ag-a.json", Learned{At: time.Unix(1, 0), Credential: "one", Answer: answer}) {
		t.Fatal("the first answer is a change")
	}
	again := readFixture(t, "fetch_available_models_full.json")
	if store.Set("ag-a.json", Learned{At: time.Unix(2, 0), Credential: "two", Answer: again}) {
		t.Fatal("the same answer, later and with another token, is no change")
	}
	if learned, _ := store.Get("ag-a.json"); learned.Credential != "two" || !learned.At.Equal(time.Unix(2, 0)) {
		t.Fatalf("learned = %+v, want the latest kept", learned)
	}
	free := readFixture(t, "fetch_available_models_free.json")
	if !store.Set("ag-a.json", Learned{Answer: free}) {
		t.Fatal("another answer is a change")
	}

	store.SetRoutes("ag-a.json", Routes(free, free.ServedIDs()))
	if route, ok := store.Route("ag-a.json", "GEMINI-4-CARBON"); !ok || route.Resolve("") != "gemini-4-carbon-high" {
		t.Fatalf("route = %+v, %v", route, ok)
	}
	if _, ok := store.Route("ag-b.json", "gemini-4-carbon"); ok {
		t.Fatal("routes are per credential")
	}
	store.SetRoutes("ag-a.json", nil)
	if _, ok := store.Route("ag-a.json", "gemini-4-carbon"); ok {
		t.Fatal("no routes removes them")
	}
	store.SetRoutes("ag-a.json", Routes(free, free.ServedIDs()))
	store.Retain(map[string]struct{}{"ag-b.json": {}})
	if _, ok := store.Get("ag-a.json"); ok {
		t.Fatal("a removed credential's answer is dropped")
	}
	if _, ok := store.Route("ag-a.json", "gemini-4-carbon"); ok {
		t.Fatal("a removed credential's routes are dropped")
	}
}

type countingLearner struct{ ensured, resolved int }

func (l *countingLearner) Ensure(context.Context) { l.ensured++ }

func (l *countingLearner) Resolve(context.Context, string) bool { l.resolved++; return true }

func TestLearnerHook(t *testing.T) {
	Ensure(context.Background())
	if Resolve(context.Background(), "gemini-4-neon") {
		t.Fatal("without a learner nothing resolves")
	}
	learner := &countingLearner{}
	SetLearner(learner)
	t.Cleanup(func() { SetLearner(nil) })
	Ensure(context.Background())
	if !Resolve(context.Background(), "gemini-4-neon") || learner.ensured != 1 || learner.resolved != 1 {
		t.Fatalf("learner = %+v", learner)
	}
}
