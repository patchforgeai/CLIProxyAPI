package cliproxy

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/authdir"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// authDirHoldWait is how long a starting router waits for another that holds its auth folder:
// one still exiting gets that long (Anvil's supervisor kills one after 3 s). Tests shorten it.
var authDirHoldWait = 15 * time.Second

// devyreSettleCredentials makes sure, as the service stops, that what the refreshes got reaches
// the token files (devyre/README.md, "No refresh is lost"): right after the refresh loop stops,
// before the API server's stop (which waits for the requests streaming), it waits a bounded while
// for the refreshes in flight (coreauth.RefreshDrainWait), then tries every save that has not
// landed once more. A restart would otherwise load refresh tokens already spent.
func (s *Service) devyreSettleCredentials() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.coreManager.DrainRefreshes(coreauth.RefreshDrainWait)
	s.coreManager.FlushPendingSaves()
}

// devyreFlushCredentials, once the API server stopped, tries once more what a request's refresh
// left unsaved since devyreSettleCredentials; with nothing pending it does nothing.
func (s *Service) devyreFlushCredentials() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.coreManager.FlushPendingSaves()
}

// devyreHoldAuthDir holds the auth folder while the service runs, so a second router (an old one
// still exiting) never refreshes its credentials beside this one: both would spend the same
// single-use refresh tokens. It waits authDirHoldWait for one that holds it, then fails the start.
func (s *Service) devyreHoldAuthDir() error {
	if s == nil || s.cfg == nil || s.cfg.AuthDir == "" {
		return nil
	}
	started := time.Now()
	lock, err := authdir.Acquire(s.cfg.AuthDir, authDirHoldWait)
	if err != nil {
		return fmt.Errorf("cliproxy: %w", err)
	}
	if waited := time.Since(started); waited > time.Second {
		log.Infof("auth folder held after waiting %s for the router that held it", waited.Round(100*time.Millisecond))
	}
	s.devyreAuthDir = lock
	return nil
}

func (s *Service) devyreReleaseAuthDir() {
	if s == nil || s.devyreAuthDir == nil {
		return
	}
	if err := s.devyreAuthDir.Release(); err != nil {
		log.Warnf("could not let go of the auth folder: %v", err)
	}
	s.devyreAuthDir = nil
}
