package auth

// devyre: a wake (devyre/README.md, "A sign-in's life"). The loop's timer never sleeps more than
// maxRefreshTimerWait, so a pass that comes far later than that means the computer slept (or the
// process was stopped), by either clock: the monotonic one stops during sleep on macOS and Linux,
// the wall clock does not. Every credential that came due meanwhile is due at once, while the
// network may still be coming back: a refresh then fails before it leaves (harmless, tried again
// in seconds) or, worse, leaves on a half-up link whose answer is lost, which may spend the
// single-use refresh token. So after a wake the due refreshes wait wakeSettle for the network.

import (
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// wakeGap is how much later than the timer allows a pass must come to read as a wake.
	wakeGap = 2 * time.Minute
	// wakeSettle is how long due refreshes wait for the network after a wake.
	wakeSettle = 20 * time.Second
)

// notePass records a pass of the loop at now, and after a gap its timer cannot explain, holds
// the refreshes due until wakeSettle from now.
func (l *authAutoRefreshLoop) notePass(now time.Time) {
	last := l.lastPass
	l.lastPass = now
	// With nothing scheduled the timer slept as long as it liked: no gap says anything then.
	if last.IsZero() || !l.timerArmed {
		return
	}
	// Sub with both readings is monotonic; without them (Round(0)) it is the wall clock's.
	elapsed := max(now.Sub(last), now.Round(0).Sub(last.Round(0)))
	if elapsed < maxRefreshTimerWait+wakeGap {
		return
	}
	l.settleUntil = now.Add(wakeSettle)
	log.WithFields(log.Fields{"asleep": elapsed.Round(time.Second).String(), "wait": wakeSettle.String()}).
		Infof("auth auto-refresh: woke after %s; refreshes due wait %s for the network", elapsed.Round(time.Second), wakeSettle)
}

// waitForNetwork puts due back until the network has had time to come back after a wake, and
// reports whether it did.
func (l *authAutoRefreshLoop) waitForNetwork(due []string, now time.Time) bool {
	if !now.Before(l.settleUntil) {
		return false
	}
	for _, authID := range due {
		l.upsert(authID, l.settleUntil)
	}
	return true
}
