package registry

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestDevyreCatalogRetryBacksOffToTheInterval(t *testing.T) {
	interval := 3 * time.Hour
	var got []time.Duration
	for failures := 1; failures <= 10; failures++ {
		got = append(got, devyreCatalogRetry(failures, interval))
	}
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, 64 * time.Minute, 128 * time.Minute, interval, interval,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
}

// The loop with a timer the test fires: each wait it asks for is recorded, and
// the next refresh runs only when the test lets it.
func TestDevyreRefreshLoopRetriesAFailedDownloadSoon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interval := 3 * time.Hour
	outcomes := []bool{false, false, true, true, false}
	labels := make(chan string, len(outcomes))
	waits := make(chan time.Duration)
	fire := make(chan chan time.Time)
	refresh := func(_ context.Context, label string) bool {
		ok := outcomes[0]
		outcomes = outcomes[1:]
		labels <- label
		return ok
	}
	after := func(d time.Duration) <-chan time.Time {
		waits <- d
		c := make(chan time.Time, 1)
		fire <- c
		return c
	}
	done := make(chan struct{})
	go func() {
		devyreRefreshLoop(ctx, interval, "start", "periodic", "test", refresh, after)
		close(done)
	}()

	expect := []struct {
		label string
		wait  time.Duration
	}{
		{"start", time.Minute},        // the start's download failed: again in a minute
		{"periodic", 2 * time.Minute}, // failed again: doubled
		{"periodic", interval},        // worked: the interval
		{"periodic", interval},        // worked again
		{"periodic", time.Minute},     // a new failure starts the backoff over
	}
	for i, step := range expect {
		if label := <-labels; label != step.label {
			t.Fatalf("refresh %d label = %q, want %q", i, label, step.label)
		}
		if wait := <-waits; wait != step.wait {
			t.Fatalf("wait after refresh %d = %s, want %s", i, wait, step.wait)
		}
		c := <-fire
		if i < len(expect)-1 {
			c <- time.Time{}
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not end with its context")
	}
}
