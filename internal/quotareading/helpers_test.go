package quotareading

import (
	"fmt"
	"testing"
	"time"
)

// testNow is the fixed clock every test in this package uses.
var testNow = time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)

func mustTime(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

func formatWindow(w Window) string {
	resets := "unknown"
	if !w.ResetsAt.IsZero() {
		resets = w.ResetsAt.UTC().Format(time.RFC3339Nano)
	}
	observed := "unknown"
	if !w.ObservedAt.IsZero() {
		observed = w.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("{%s %s used=%g resets=%s len=%s model=%q observed=%s source=%s}",
		w.ID, w.Kind, w.UsedPercent, resets, w.Length, w.Model, observed, w.Source)
}

func windowsEqual(a, b Window) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.UsedPercent == b.UsedPercent &&
		a.ResetsAt.Equal(b.ResetsAt) && a.Length == b.Length && a.Model == b.Model &&
		a.ObservedAt.Equal(b.ObservedAt) && a.Source == b.Source
}

func assertWindows(t *testing.T, got, want []Window) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d\n got: %v\nwant: %v", len(got), len(want), formatWindows(got), formatWindows(want))
	}
	for i := range want {
		if !windowsEqual(got[i], want[i]) {
			t.Fatalf("window %d mismatch\n got: %s\nwant: %s", i, formatWindow(got[i]), formatWindow(want[i]))
		}
	}
}

func formatWindows(ws []Window) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, formatWindow(w))
	}
	return out
}
