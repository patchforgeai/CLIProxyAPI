package auth

// devyre: a wake (devyre/README.md, "A sign-in's life"). The loop's timer never sleeps more than
// maxRefreshTimerWait, so a pass that comes far later than that means the computer slept (or the
// process was stopped), by either clock: the monotonic one stops during sleep on macOS and Linux,
// the wall clock does not. Every credential that came due meanwhile is due at once, while the
// network may still be coming back: a refresh then fails before it leaves (harmless, tried again
// in seconds) or, worse, leaves on a half-up link whose answer is lost, which may spend the
// single-use refresh token. So after a wake the due refreshes wait for the network: until a token
// endpoint answers a connection (SetNetworkProbe), wakeSettle at most.

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// wakeGap is how much later than the timer allows a pass must come to read as a wake.
	wakeGap = 2 * time.Minute
	// wakeSettle is the longest due refreshes wait for the network after a wake.
	wakeSettle = 20 * time.Second
	// wakeProbeTimeout bounds one look at the network.
	wakeProbeTimeout = 2 * time.Second
)

// wakeProbeEvery is how often the network is looked at during the wait (tests shorten it).
var wakeProbeEvery = time.Second

// wakeWait is the wait for the network after a wake: until when, and the credentials it holds.
type wakeWait struct {
	mu    sync.Mutex
	until time.Time
	held  map[string]struct{}
}

// SetNetworkProbe gives the refresh loop a look at the network after a wake: true once a token
// endpoint answers a connection, which ends the wait for the network early. Without one the wait
// is wakeSettle.
func (m *Manager) SetNetworkProbe(probe func(ctx context.Context) bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.devyre.networkUp = probe
	m.mu.Unlock()
}

// notePass records a pass of the loop at now, and after a gap its timer cannot explain, holds
// the refreshes due until wakeSettle from now, or until the network answers.
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
	until := now.Add(wakeSettle)
	l.wake.mu.Lock()
	l.wake.until = until
	l.wake.held = make(map[string]struct{})
	l.wake.mu.Unlock()
	log.WithFields(log.Fields{"asleep": elapsed.Round(time.Second).String(), "wait": wakeSettle.String()}).
		Infof("auth auto-refresh: woke after %s; refreshes due wait up to %s for the network", elapsed.Round(time.Second), wakeSettle)
	var probe func(context.Context) bool
	if l.manager != nil {
		l.manager.mu.RLock()
		probe = l.manager.devyre.networkUp
		l.manager.mu.RUnlock()
	}
	if probe != nil {
		go l.lookForNetwork(probe, until)
	}
}

// lookForNetwork looks at the network every wakeProbeEvery until the wait that ends at until is
// over; once it answers, the wait ends and the refreshes it held are due at once.
func (l *authAutoRefreshLoop) lookForNetwork(probe func(context.Context) bool, until time.Time) {
	for time.Now().Before(until) {
		ctx, cancel := context.WithTimeout(context.Background(), wakeProbeTimeout)
		up := probe(ctx)
		cancel()
		if up {
			l.endWait(until)
			return
		}
		time.Sleep(wakeProbeEvery)
	}
}

// endWait ends the wait that was to end at until, if it is still the one, and makes what it held
// due at once.
func (l *authAutoRefreshLoop) endWait(until time.Time) {
	l.wake.mu.Lock()
	if !l.wake.until.Equal(until) {
		l.wake.mu.Unlock()
		return
	}
	held := l.wake.held
	l.wake.until, l.wake.held = time.Time{}, nil
	l.wake.mu.Unlock()
	log.WithFields(log.Fields{"waited": (wakeSettle - time.Until(until)).Round(time.Second).String()}).
		Info("auth auto-refresh: the network answers; refreshes due go out")
	for authID := range held {
		l.queueReschedule(authID)
	}
}

// waitForNetwork puts due back until the wait for the network after a wake ends, and reports
// whether it did.
func (l *authAutoRefreshLoop) waitForNetwork(due []string, now time.Time) bool {
	l.wake.mu.Lock()
	until := l.wake.until
	if !now.Before(until) {
		l.wake.mu.Unlock()
		return false
	}
	for _, authID := range due {
		l.wake.held[authID] = struct{}{}
	}
	l.wake.mu.Unlock()
	for _, authID := range due {
		l.upsert(authID, until)
	}
	return true
}
