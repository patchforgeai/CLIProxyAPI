package quotareading

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// unixMillisThreshold separates unix seconds from unix milliseconds by
// magnitude: seconds stay near 1e9 for centuries, milliseconds are near 1e12.
const unixMillisThreshold = 1e11

// FromSignals parses the passive header snapshot of one credential. Only the
// "claude" and "codex" providers carry quota windows; others return nil.
func FromSignals(provider string, signals map[string]string, observedAt time.Time) []Window {
	switch normalizeProvider(provider) {
	case "claude":
		return FromClaudeHeaders(signals, observedAt)
	case "codex":
		return FromCodexHeaders(signals, observedAt)
	default:
		return nil
	}
}

// FromUsageBody parses a usage body fetched from rawURL. The Claude and Codex
// usage endpoints are recognized by host and path; any other URL, such as the
// Claude profile endpoint, returns nil, nil. A non-empty provider that does not
// own the endpoint also returns nil, nil so a body is never filed under the
// wrong provider.
func FromUsageBody(provider, rawURL string, body []byte, observedAt time.Time) ([]Window, error) {
	endpoint := usageEndpointProvider(rawURL)
	if endpoint == "" {
		return nil, nil
	}
	if p := normalizeProvider(provider); p != "" && p != endpoint {
		return nil, nil
	}
	if endpoint == "claude" {
		return FromClaudeUsage(body, observedAt)
	}
	return FromCodexUsage(body, observedAt)
}

// usageEndpointProvider names the provider whose usage endpoint rawURL targets.
func usageEndpointProvider(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case host == "api.anthropic.com" && path == "/api/oauth/usage":
		return "claude"
	case host == "chatgpt.com" && path == "/backend-api/wham/usage":
		return "codex"
	default:
		return ""
	}
}

// signalLookup indexes a signal snapshot by lowercased header name.
// collectQuotaSignals stores canonical header names, so lookups must ignore
// case. When two stored names differ only by case, the lexicographically
// greatest original name wins so the result never depends on map order.
type signalLookup map[string]signalValue

type signalValue struct {
	name  string
	value string
}

func newSignalLookup(signals map[string]string, prefix string) signalLookup {
	if len(signals) == 0 {
		return nil
	}
	lookup := make(signalLookup)
	for name, value := range signals {
		lower := strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		if existing, ok := lookup[lower]; ok && existing.name > name {
			continue
		}
		lookup[lower] = signalValue{name: name, value: strings.TrimSpace(value)}
	}
	return lookup
}

func (l signalLookup) get(name string) string {
	return l[name].value
}

// parseFinite parses a decimal number, rejecting NaN and infinities.
func parseFinite(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// usedPercentValue validates an observed usage percentage. Negative values are
// not meaningful observations and are rejected; values above 100 clamp to 100.
func usedPercentValue(value float64, ok bool) (float64, bool) {
	if !ok || value < 0 {
		return 0, false
	}
	return clampPercent(value), true
}

// parseInstant accepts unix seconds (fractional allowed), unix milliseconds,
// RFC 3339 and HTTP dates. Unparseable or non-positive values return zero.
func parseInstant(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if value, err := strconv.ParseFloat(raw, 64); err == nil {
		return unixInstant(value)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC()
	}
	if parsed, err := http.ParseTime(raw); err == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

// unixInstant converts unix seconds, or milliseconds by magnitude, to a time.
func unixInstant(value float64) time.Time {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return time.Time{}
	}
	if value >= unixMillisThreshold {
		value /= 1000
	}
	seconds := math.Floor(value)
	nanos := math.Round((value - seconds) * 1e9)
	return time.Unix(int64(seconds), int64(nanos)).UTC()
}

// parseOffset converts a non-negative "seconds from observedAt" value. The
// result is unknown when observedAt is unknown.
func parseOffset(raw string, observedAt time.Time) time.Time {
	seconds, ok := parseFinite(raw)
	if !ok || seconds < 0 || observedAt.IsZero() {
		return time.Time{}
	}
	return observedAt.Add(secondsDuration(seconds))
}

// secondsDuration converts seconds to a duration, saturating instead of overflowing.
func secondsDuration(seconds float64) time.Duration {
	nanos := seconds * float64(time.Second)
	if nanos >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(nanos)
}

// jsonFinite reads a JSON number or numeric string.
func jsonFinite(value gjson.Result) (float64, bool) {
	switch value.Type {
	case gjson.Number:
		if math.IsNaN(value.Num) || math.IsInf(value.Num, 0) {
			return 0, false
		}
		return value.Num, true
	case gjson.String:
		return parseFinite(value.Str)
	default:
		return 0, false
	}
}

// jsonInstant reads an instant given as an RFC 3339 string or as unix seconds
// or milliseconds (number or numeric string).
func jsonInstant(value gjson.Result) time.Time {
	switch value.Type {
	case gjson.String:
		return parseInstant(value.Str)
	case gjson.Number:
		return unixInstant(value.Num)
	default:
		return time.Time{}
	}
}

// jsonFirst returns the first present, non-null member among names.
func jsonFirst(object gjson.Result, names ...string) gjson.Result {
	for _, name := range names {
		value := object.Get(gjson.Escape(name))
		if value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}

// sortWindows orders windows by ID, the order every Reading uses.
func sortWindows(windows []Window) {
	sort.Slice(windows, func(i, j int) bool { return windows[i].ID < windows[j].ID })
}
