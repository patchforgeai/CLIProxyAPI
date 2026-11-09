package registry

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// devyre: a model catalog download that fails is tried again soon, not after
// the full refresh interval.
//
// Upstream downloads each remote catalog (models.json, the Codex client
// catalog, Devin's) once at start and then every modelsRefreshInterval (3 h),
// whether or not the start's download worked. A router started before its
// network is up (at sign-in, on a laptop waking, behind a filter that comes up
// late) then serves the embedded catalog for three hours: every model released
// after the build is unknown, and refused with "unknown provider for model".
// After a failure the next try now comes after devyreCatalogRetryFirst,
// doubling up to the interval; after a success, after the interval as before.
const devyreCatalogRetryFirst = time.Minute

// devyreCatalogRetry is the wait after the failures-th failure in a row.
func devyreCatalogRetry(failures int, interval time.Duration) time.Duration {
	wait := devyreCatalogRetryFirst
	for i := 1; i < failures && wait < interval; i++ {
		wait *= 2
	}
	return min(wait, interval)
}

// devyreRefreshLoop runs refresh at once (startLabel) and again
// (periodicLabel) after the interval following a success, or after the
// backoff following a failure, until ctx ends. after makes the timers (tests
// pass their own).
func devyreRefreshLoop(
	ctx context.Context,
	interval time.Duration,
	startLabel, periodicLabel, name string,
	refresh func(ctx context.Context, label string) bool,
	after func(time.Duration) <-chan time.Time,
) {
	ok := refresh(ctx, startLabel)
	log.Infof("periodic %s refresh started (interval=%s)", name, interval)
	failures := 0
	for {
		wait := interval
		if ok {
			failures = 0
		} else {
			failures++
			wait = devyreCatalogRetry(failures, interval)
			log.Infof("%s: trying again in %s", name, wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
			ok = refresh(ctx, periodicLabel)
		}
	}
}
