package cliproxy

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// devyre: session bindings across routing reloads and restarts.

// devyreSessionState holds the persistence of the session bindings.
type devyreSessionState struct {
	mu sync.Mutex
	// started is set once Run restored the bindings; config applies before
	// that change nothing here.
	started     bool
	persistence *coreauth.SessionPersistence
}

// devyreRoutingSelector builds the selector for state, as newRoutingSelector
// does, and carries the session bindings of the current selector over. A
// routing change in a config reload rebuilds the selector, and the old one's
// cache, every binding in it, went with it: each session's next request was
// placed anew, many on another account, which then had to cache the whole
// context again.
func (s *Service) devyreRoutingSelector(state routingRuntimeState) coreauth.Selector {
	selector := newRoutingSelector(state)
	if carried := coreauth.CarrySessionBindings(s.coreManager.Selector(), selector); carried > 0 {
		log.Infof("session-affinity: kept %d bindings across the routing reload", carried)
	}
	return selector
}

// devyreSessionAffinityPath returns where the session bindings persist: in the
// writable path (WRITABLE_PATH) when it is set, else next to the config file.
// It is empty without either.
func devyreSessionAffinityPath(configPath string) string {
	if dir := util.WritablePath(); dir != "" {
		return filepath.Join(dir, coreauth.SessionAffinityFileName)
	}
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), coreauth.SessionAffinityFileName)
}

// devyreStartSessionPersistence restores the saved session bindings into the
// selector and starts saving them, when routing.session-affinity-persist and
// session affinity are on. Run calls it once, before the server takes requests.
func (s *Service) devyreStartSessionPersistence() {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	state := &s.devyreSessions
	state.mu.Lock()
	defer state.mu.Unlock()
	state.started = true
	s.devyreSyncSessionPersistenceLocked(cfg, true)
}

// devyreApplySessionPersistence follows a config apply after Run started: it
// starts or stops saving as the setting changes and follows a rebuilt
// selector. Turned on at run time, it saves the live bindings without loading
// the file.
func (s *Service) devyreApplySessionPersistence(cfg *config.Config) {
	state := &s.devyreSessions
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started {
		s.devyreSyncSessionPersistenceLocked(cfg, false)
	}
}

// devyreStopSessionPersistence saves the session bindings once more and stops
// saving them. Shutdown calls it once the server stopped, after the stdin
// lifeline's shutdown line, its EOF and a signal alike.
func (s *Service) devyreStopSessionPersistence() {
	state := &s.devyreSessions
	state.mu.Lock()
	defer state.mu.Unlock()
	state.started = false
	if state.persistence != nil {
		state.persistence.Close()
		state.persistence = nil
	}
}

// devyreSyncSessionPersistenceLocked makes the persistence match cfg and the
// current selector. Callers hold s.devyreSessions.mu.
func (s *Service) devyreSyncSessionPersistenceLocked(cfg *config.Config, load bool) {
	state := &s.devyreSessions
	var selector coreauth.Selector
	if s.coreManager != nil {
		selector = s.coreManager.Selector()
	}
	_, affinity := selector.(*coreauth.SessionAffinitySelector)
	enabled := affinity && cfg != nil && !cfg.Home.Enabled && cfg.Routing.SessionAffinityPersistEnabled()
	if !enabled {
		if state.persistence != nil {
			state.persistence.Close()
			state.persistence = nil
		}
		return
	}
	if state.persistence == nil {
		path := devyreSessionAffinityPath(s.configPath)
		if path == "" {
			log.Debug("session-affinity: no writable path or config folder, bindings stay in memory")
			return
		}
		state.persistence = coreauth.NewSessionPersistence(path)
		if load {
			state.persistence.Load(selector)
		}
		state.persistence.Attach(selector)
		if !load {
			state.persistence.Changed()
		}
		return
	}
	state.persistence.Attach(selector)
}
