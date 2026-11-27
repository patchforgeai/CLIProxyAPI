package auth

// devyre: a shutdown waits for the refreshes in flight (devyre/README.md, "No refresh is lost").
// A refresh's request outlives its caller (internal/auth/tokenrefresh), but the process did not:
// Shutdown only cancelled the refresh loop and returned in microseconds while a refresh was on
// the wire (REPORT 3a, the probe), and the process exited before its answer was saved, which
// leaves the spent refresh token on disk. Now Shutdown waits for them, a bounded while that the
// supervisor's stop grace allows (Anvil's supervisor.ts, `stopGraceMs`).

import (
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// RefreshDrainWait is how long a shutdown waits for the refreshes in flight: a refresh's exchange
// takes about a second (its own bound, 30 s, is longer than a stop may take), and Anvil's
// supervisor gives the router 3 s to stop before it kills it (supervisor.ts `stopGraceMs`), within
// the server's own 5 s stop budget.
const RefreshDrainWait = 2 * time.Second

// devyreRefreshDrain counts the refreshes in flight, by credential.
type devyreRefreshDrain struct {
	mu       sync.Mutex
	inFlight map[string]int
	idle     chan struct{}
}

func (d *devyreRefreshDrain) begin(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inFlight == nil {
		d.inFlight = make(map[string]int)
	}
	if len(d.inFlight) == 0 {
		d.idle = make(chan struct{})
	}
	d.inFlight[id]++
}

func (d *devyreRefreshDrain) end(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inFlight[id] <= 1 {
		delete(d.inFlight, id)
	} else {
		d.inFlight[id]--
	}
	if len(d.inFlight) == 0 && d.idle != nil {
		close(d.idle)
		d.idle = nil
	}
}

// DrainRefreshes waits up to wait for the refreshes in flight to land, and returns the
// credentials of those still in flight after it, which a shutdown then abandons.
func (m *Manager) DrainRefreshes(wait time.Duration) []string {
	if m == nil {
		return nil
	}
	d := &m.devyre.refreshes
	d.mu.Lock()
	idle := d.idle
	waiting := len(d.inFlight)
	d.mu.Unlock()
	if idle == nil {
		return nil
	}
	started := time.Now()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-idle:
		log.WithFields(log.Fields{"refreshes": waiting, "waited": time.Since(started).Round(time.Millisecond).String()}).
			Infof("shutdown waited for %d refresh(es) in flight", waiting)
		return nil
	case <-timer.C:
	}
	d.mu.Lock()
	left := make([]string, 0, len(d.inFlight))
	for id := range d.inFlight {
		left = append(left, id)
	}
	d.mu.Unlock()
	sort.Strings(left)
	for _, id := range left {
		log.WithFields(log.Fields{"credential": id, "step": "shutdown"}).
			Warnf("the refresh of %s is still in flight after %s at shutdown: its answer may be lost", id, wait)
	}
	return left
}
