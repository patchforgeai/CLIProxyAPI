package quotareading

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const codexHeaderPrefix = "x-codex-"

// codexWindowIDs are the credential-level Codex windows. Additional (per
// model) and code-review limits are not used for routing.
var codexWindowIDs = []string{"primary", "secondary"}

// FromCodexHeaders parses the x-codex-{primary,secondary}-* signals.
// used-percent is a percentage; the reset comes from reset-at (unix seconds)
// or, failing that, from reset-after-seconds relative to observedAt;
// window-minutes gives the length. A window without used-percent counts as
// 100 percent used only when the snapshot says the limit is reached and the
// window has a reset, mirroring the panel's Codex parser.
func FromCodexHeaders(signals map[string]string, observedAt time.Time) []Window {
	lookup := newSignalLookup(signals, codexHeaderPrefix)
	if len(lookup) == 0 {
		return nil
	}
	limitReached := strings.EqualFold(lookup.get("x-codex-limit-reached"), "true") ||
		strings.EqualFold(lookup.get("x-codex-allowed"), "false")
	var windows []Window
	for _, id := range codexWindowIDs {
		prefix := codexHeaderPrefix + id + "-"
		resetsAt := parseInstant(lookup.get(prefix + "reset-at"))
		if resetsAt.IsZero() {
			resetsAt = parseOffset(lookup.get(prefix+"reset-after-seconds"), observedAt)
		}
		var length time.Duration
		if minutes, ok := parseFinite(lookup.get(prefix + "window-minutes")); ok && minutes > 0 {
			length = secondsDuration(minutes * 60)
		}
		used, ok := usedPercentValue(parseFinite(lookup.get(prefix + "used-percent")))
		if !ok && limitReached && !resetsAt.IsZero() {
			used, ok = 100, true
		}
		if !ok {
			continue
		}
		windows = append(windows, codexWindow(id, used, resetsAt, length, observedAt, SourceHeader))
	}
	sortWindows(windows)
	return windows
}

// FromCodexUsage parses a GET /backend-api/wham/usage body:
// rate_limit.{primary_window,secondary_window}{used_percent, reset_at,
// reset_after_seconds, limit_window_seconds}, accepting the camelCase
// spellings too. reset_at is unix seconds, the unit the panel formats it in
// (formatUnixSeconds); a millisecond value is recognized by magnitude.
func FromCodexUsage(body []byte, observedAt time.Time) ([]Window, error) {
	root, err := parseUsageObject(body, "codex")
	if err != nil {
		return nil, err
	}
	return codexUsageWindowsFrom(root, observedAt), nil
}

// codexUsageWindowsFrom implements FromCodexUsage on a parsed JSON object.
func codexUsageWindowsFrom(root gjson.Result, observedAt time.Time) []Window {
	rateLimit := jsonFirst(root, "rate_limit", "rateLimit")
	if !rateLimit.IsObject() {
		return nil
	}
	limitReached := jsonFirst(rateLimit, "limit_reached", "limitReached").Type == gjson.True ||
		jsonFirst(rateLimit, "allowed").Type == gjson.False
	var windows []Window
	for _, id := range codexWindowIDs {
		entry := jsonFirst(rateLimit, id+"_window", id+"Window")
		if !entry.IsObject() {
			continue
		}
		resetsAt := jsonInstant(jsonFirst(entry, "reset_at", "resetAt"))
		if resetsAt.IsZero() {
			resetsAt = parseOffset(jsonFirst(entry, "reset_after_seconds", "resetAfterSeconds").String(), observedAt)
		}
		var length time.Duration
		if seconds, ok := jsonFinite(jsonFirst(entry, "limit_window_seconds", "limitWindowSeconds")); ok && seconds > 0 {
			length = secondsDuration(seconds)
		}
		used, ok := usedPercentValue(jsonFinite(jsonFirst(entry, "used_percent", "usedPercent")))
		if !ok && limitReached && !resetsAt.IsZero() {
			used, ok = 100, true
		}
		if !ok {
			continue
		}
		windows = append(windows, codexWindow(id, used, resetsAt, length, observedAt, SourceUsage))
	}
	sortWindows(windows)
	return windows
}

// CodexUsageHasRoom reports whether a GET /backend-api/wham/usage body says the
// account is at none of its limits, so that a request ChatGPT rejected for a
// usage limit before would be served now. That takes all of:
//
//   - rate_limit lists at least one window, is not limit_reached and not
//     disallowed, and every window it lists is below 100 percent used;
//   - every entry of additional_rate_limits (a model's own limit, such as
//     GPT-5.3-Codex-Spark's) says the same of its own rate_limit, where it has
//     one. Which limit rejected a request is not known afterwards, so one of
//     them still reached leaves the question open.
//
// A window without a usable used_percent, a body that is not the endpoint's
// usage shape, and anything malformed is no evidence of room. The code-review
// limit and the credits take no part: they reject no model request.
func CodexUsageHasRoom(body []byte) bool {
	root, err := parseUsageObject(body, "codex")
	if err != nil {
		return false
	}
	rateLimit := jsonFirst(root, "rate_limit", "rateLimit")
	if !rateLimit.IsObject() {
		return false
	}
	if windows, ok := codexRateLimitRoom(rateLimit); !ok || windows == 0 {
		return false
	}
	room := true
	jsonFirst(root, "additional_rate_limits", "additionalRateLimits").ForEach(func(_, entry gjson.Result) bool {
		limit := jsonFirst(entry, "rate_limit", "rateLimit")
		if !limit.IsObject() {
			// The entry itself holds the limit where it is keyed by its name (the
			// websocket's shape); one with no limit of its own lists nothing.
			limit = entry
		}
		if limit.IsObject() {
			_, room = codexRateLimitRoom(limit)
		}
		return room
	})
	return room
}

// codexRateLimitRoom reads one rate_limit object: how many windows it lists,
// and whether it leaves room: not limit_reached, not disallowed, and every
// listed window with a used_percent below 100.
func codexRateLimitRoom(rateLimit gjson.Result) (windows int, ok bool) {
	if jsonFirst(rateLimit, "limit_reached", "limitReached").Type == gjson.True ||
		jsonFirst(rateLimit, "allowed").Type == gjson.False {
		return 0, false
	}
	for _, id := range codexWindowIDs {
		entry := jsonFirst(rateLimit, id+"_window", id+"Window")
		if !entry.IsObject() {
			continue
		}
		used, known := usedPercentValue(jsonFinite(jsonFirst(entry, "used_percent", "usedPercent")))
		if !known || used >= 100 {
			return windows, false
		}
		windows++
	}
	return windows, true
}

// codexWindow classifies a Codex window by length: up to 24 hours gates, longer
// ranks. With an unknown length, primary is the short and secondary the long window.
func codexWindow(id string, used float64, resetsAt time.Time, length time.Duration, observedAt time.Time, source Source) Window {
	kind := kindForLength(length)
	if length <= 0 {
		kind = KindShort
		if id == "secondary" {
			kind = KindLong
		}
	}
	return Window{
		ID:          id,
		Kind:        kind,
		UsedPercent: used,
		ResetsAt:    resetsAt,
		Length:      length,
		ObservedAt:  observedAt,
		Source:      source,
	}
}
