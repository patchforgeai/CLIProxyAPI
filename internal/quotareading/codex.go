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
	rateLimit := jsonFirst(root, "rate_limit", "rateLimit")
	if !rateLimit.IsObject() {
		return nil, nil
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
	return windows, nil
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
