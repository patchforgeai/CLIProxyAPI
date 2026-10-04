package quotareading

import (
	"errors"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	claudeShortWindow = 5 * time.Hour
	claudeWeekWindow  = 7 * 24 * time.Hour

	claudeHeaderPrefix = "anthropic-ratelimit-unified-"
)

// claudeWindowSpec maps one Claude window name to its routing identity.
type claudeWindowSpec struct {
	name   string
	id     string
	kind   Kind
	model  string
	length time.Duration
}

// claudeHeaderWindows lists the unified rate-limit windows in response headers.
// 7d_oi is the Fable-specific weekly window, the same reading upstream
// helps/claude_ratelimit.go gives it.
var claudeHeaderWindows = []claudeWindowSpec{
	{name: "5h", id: "5h", kind: KindShort, length: claudeShortWindow},
	{name: "7d", id: "7d", kind: KindLong, length: claudeWeekWindow},
	{name: "7d_oi", id: "7d:fable", kind: KindScoped, model: "fable", length: claudeWeekWindow},
}

// claudeUsageWindows lists the named windows of GET /api/oauth/usage that
// routing uses. iguana_necktie is the legacy Fable key. seven_day_oauth_apps,
// seven_day_cowork and extra_usage are deliberately ignored.
var claudeUsageWindows = []claudeWindowSpec{
	{name: "five_hour", id: "5h", kind: KindShort, length: claudeShortWindow},
	{name: "seven_day", id: "7d", kind: KindLong, length: claudeWeekWindow},
	{name: "seven_day_opus", id: "7d:opus", kind: KindScoped, model: "opus", length: claudeWeekWindow},
	{name: "seven_day_sonnet", id: "7d:sonnet", kind: KindScoped, model: "sonnet", length: claudeWeekWindow},
	{name: "iguana_necktie", id: "7d:fable", kind: KindScoped, model: "fable", length: claudeWeekWindow},
}

// FromClaudeHeaders parses the anthropic-ratelimit-unified-{5h,7d,7d_oi}-*
// signals. Utilization is a fraction (0.69 means 69 percent; values above 1
// clamp to 100) and reset is unix seconds or RFC 3339. A "rejected" status
// marks the window exhausted: 100 percent used, whatever utilization says.
// A window without a usable utilization and not rejected is skipped.
func FromClaudeHeaders(signals map[string]string, observedAt time.Time) []Window {
	lookup := newSignalLookup(signals, claudeHeaderPrefix)
	if len(lookup) == 0 {
		return nil
	}
	var windows []Window
	for _, spec := range claudeHeaderWindows {
		prefix := claudeHeaderPrefix + spec.name + "-"
		fraction, ok := parseFinite(lookup.get(prefix + "utilization"))
		used, ok := usedPercentValue(fraction*100, ok)
		if strings.EqualFold(lookup.get(prefix+"status"), "rejected") {
			used, ok = 100, true
		}
		if !ok {
			continue
		}
		windows = append(windows, Window{
			ID:          spec.id,
			Kind:        spec.kind,
			UsedPercent: used,
			ResetsAt:    parseInstant(lookup.get(prefix + "reset")),
			Length:      spec.length,
			Model:       spec.model,
			ObservedAt:  observedAt,
			Source:      SourceHeader,
		})
	}
	sortWindows(windows)
	return windows
}

// FromClaudeUsage parses a GET /api/oauth/usage body. Named windows carry
// {utilization: percent 0..100, resets_at: RFC 3339 or null}. limits[] entries
// of kind "weekly_scoped" become "7d:<family>" scoped windows using "percent";
// for one family the active entry wins, else the first valid one. A valid
// limits[] window replaces the named window with the same ID, so the legacy
// iguana_necktie key only counts when limits[] has no Fable entry.
func FromClaudeUsage(body []byte, observedAt time.Time) ([]Window, error) {
	root, err := parseUsageObject(body, "claude")
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Window)
	for _, spec := range claudeUsageWindows {
		entry := root.Get(spec.name)
		if !entry.IsObject() {
			continue
		}
		used, ok := usedPercentValue(jsonFinite(entry.Get("utilization")))
		if !ok {
			continue
		}
		byID[spec.id] = Window{
			ID:          spec.id,
			Kind:        spec.kind,
			UsedPercent: used,
			ResetsAt:    jsonInstant(entry.Get("resets_at")),
			Length:      spec.length,
			Model:       spec.model,
			ObservedAt:  observedAt,
			Source:      SourceUsage,
		}
	}
	for _, window := range claudeScopedLimits(root.Get("limits"), observedAt) {
		byID[window.ID] = window
	}
	if len(byID) == 0 {
		return nil, nil
	}
	windows := make([]Window, 0, len(byID))
	for _, window := range byID {
		windows = append(windows, window)
	}
	sortWindows(windows)
	return windows, nil
}

// claudeScopedLimits selects one weekly_scoped limit per model family. Entries
// with an unknown family or without a numeric percent are skipped, matching
// the panel parser so a malformed modern entry falls back to the legacy key.
func claudeScopedLimits(limits gjson.Result, observedAt time.Time) []Window {
	if !limits.IsArray() {
		return nil
	}
	type candidate struct {
		window Window
		active bool
	}
	chosen := make(map[string]candidate)
	var order []string
	limits.ForEach(func(_, entry gjson.Result) bool {
		if !entry.IsObject() {
			return true
		}
		if !strings.EqualFold(strings.TrimSpace(entry.Get("kind").String()), "weekly_scoped") {
			return true
		}
		family := ModelFamily(entry.Get("scope.model.display_name").String())
		if family == "" {
			return true
		}
		used, ok := usedPercentValue(jsonFinite(entry.Get("percent")))
		if !ok {
			return true
		}
		next := candidate{
			window: Window{
				ID:          "7d:" + family,
				Kind:        KindScoped,
				UsedPercent: used,
				ResetsAt:    jsonInstant(entry.Get("resets_at")),
				Length:      claudeWeekWindow,
				Model:       family,
				ObservedAt:  observedAt,
				Source:      SourceUsage,
			},
			active: entry.Get("is_active").Type == gjson.True,
		}
		previous, seen := chosen[family]
		if !seen {
			order = append(order, family)
		}
		if !seen || (next.active && !previous.active) {
			chosen[family] = next
		}
		return true
	})
	windows := make([]Window, 0, len(order))
	for _, family := range order {
		windows = append(windows, chosen[family].window)
	}
	return windows
}

// parseUsageObject validates that body is a JSON object. Errors never include
// the body, which may carry account details.
func parseUsageObject(body []byte, provider string) (gjson.Result, error) {
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, errors.New("quotareading: " + provider + " usage body is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return gjson.Result{}, errors.New("quotareading: " + provider + " usage body is not a JSON object")
	}
	return root, nil
}
