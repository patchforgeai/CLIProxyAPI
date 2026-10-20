package cliproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// devyreSessionOpts is a Claude Code request of session id.
func devyreSessionOpts(id string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{id}}, Metadata: map[string]any{}}
}

var devyrePersistAuths = []*coreauth.Auth{{ID: "persist-svc-a", Provider: "claude"}, {ID: "persist-svc-b", Provider: "claude"}}

// devyrePersistPick binds session on the service's selector.
func devyrePersistPick(t *testing.T, service *Service, session string) string {
	t.Helper()
	got, err := service.coreManager.Selector().Pick(context.Background(), "claude", "claude-opus-5-5", devyreSessionOpts(session), devyrePersistAuths)
	if err != nil {
		t.Fatal(err)
	}
	return got.ID
}

// devyreLookup returns the credential session is bound to on the service's selector.
func devyreLookup(service *Service, session string) string {
	affinity, ok := service.coreManager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok {
		return ""
	}
	authID, status := affinity.LookupAffinity("claude", "claude-opus-5-5", "claude:"+session)
	if status != "bound" {
		return ""
	}
	return authID
}

// devyrePersistService is a service with cfg's selector, as Build makes it.
func devyrePersistService(cfg *internalconfig.Config, configPath string) *Service {
	state := normalizedRoutingRuntimeState(cfg)
	return &Service{cfg: cfg, configPath: configPath, coreManager: coreauth.NewManager(nil, newRoutingSelector(state), nil), appliedRoutingState: &state}
}

// The bindings persist across restarts through the service's own start and
// stop: Run restores them before the server takes requests, and the stop
// that a cancelled run context triggers (the stdin lifeline's shutdown line
// and EOF cancel it) saves them.
func TestDevyreSessionPersistenceAcrossRuns(t *testing.T) {
	writable := t.TempDir()
	t.Setenv("WRITABLE_PATH", writable)
	run := func(stage string, during func(service *Service)) {
		t.Helper()
		listener, errListen := net.Listen("tcp", "127.0.0.1:0")
		if errListen != nil {
			t.Fatal(errListen)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if errClose := listener.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		authDir := t.TempDir()
		cfgPath := filepath.Join(authDir, "config.yaml")
		if errWrite := os.WriteFile(cfgPath, []byte("{}"), 0o644); errWrite != nil {
			t.Fatal(errWrite)
		}
		cfg := devyreAffinityConfig(2, "1h")
		cfg.Host, cfg.Port, cfg.AuthDir = "127.0.0.1", port, authDir
		started := make(chan *Service, 1)
		service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(cfgPath).
			WithHooks(Hooks{OnAfterStart: func(s *Service) { started <- s }}).Build()
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- service.Run(ctx) }()
		select {
		case <-started:
		case errRun := <-done:
			t.Fatalf("%s: Run() ended before start: %v", stage, errRun)
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the service did not start", stage)
		}
		during(service)
		cancel()
		select {
		case errRun := <-done:
			if errRun != nil && !errors.Is(errRun, context.Canceled) {
				t.Fatalf("%s: Run() = %v", stage, errRun)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: Run() did not stop", stage)
		}
	}

	var bound string
	run("first run", func(service *Service) {
		bound = devyrePersistPick(t, service, "across-runs")
	})
	if _, errStat := os.Stat(filepath.Join(writable, coreauth.SessionAffinityFileName)); errStat != nil {
		t.Fatalf("the stop did not save the bindings in WRITABLE_PATH: %v", errStat)
	}
	run("second run", func(service *Service) {
		if got := devyreLookup(service, "across-runs"); got != bound {
			t.Fatalf("after a restart the session is bound to %q, want %s", got, bound)
		}
	})
}

func TestDevyreSessionPersistencePath(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join("data", "router", "config.yaml")
	if got, want := devyreSessionAffinityPath(configPath), filepath.Join("data", "router", coreauth.SessionAffinityFileName); got != want {
		t.Fatalf("path next to the config = %q, want %q", got, want)
	}
	if got := devyreSessionAffinityPath(""); got != "" {
		t.Fatalf("path without a config file = %q, want none", got)
	}
	writable := t.TempDir()
	t.Setenv("WRITABLE_PATH", writable)
	if got, want := devyreSessionAffinityPath(configPath), filepath.Join(writable, coreauth.SessionAffinityFileName); got != want {
		t.Fatalf("path with WRITABLE_PATH = %q, want %q", got, want)
	}
}

// routing.session-affinity-persist turns saving off and on at run time, and the
// persistence follows a selector rebuilt by a reload.
func TestDevyreSessionPersistenceFollowsTheConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(dir, "config.yaml")
	file := filepath.Join(dir, coreauth.SessionAffinityFileName)
	off := false
	cfg := devyreAffinityConfig(2, "1h")
	cfg.Routing.SessionAffinityPersist = &off
	service := devyrePersistService(cfg, configPath)
	service.devyreStartSessionPersistence()
	devyrePersistPick(t, service, "unsaved")
	if service.devyreSessions.persistence != nil {
		t.Fatal("session-affinity-persist: false persists")
	}

	apply := func(sequence uint64, cfg *internalconfig.Config) {
		t.Helper()
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: sequence}) {
			t.Fatalf("applyManagerConfig(#%d) failed", sequence)
		}
	}
	apply(1, devyreAffinityConfig(2, "1h")) // persist back on: the live bindings are saved
	apply(2, devyreAffinityConfig(5, "1h")) // a rebuilt selector, carried and followed
	bound := devyrePersistPick(t, service, "after-reload")
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatal(errShutdown)
	}
	restarted := devyrePersistService(devyreAffinityConfig(2, "1h"), configPath)
	restarted.devyreStartSessionPersistence()
	defer restarted.devyreStopSessionPersistence()
	if got := devyreLookup(restarted, "after-reload"); got != bound {
		t.Fatalf("after-reload is bound to %q after a restart, want %s", got, bound)
	}
	if got := devyreLookup(restarted, "unsaved"); got == "" {
		t.Fatal("the binding made while saving was off was not saved once it was back on")
	}

	// Without session affinity nothing persists.
	if errRemove := os.Remove(file); errRemove != nil {
		t.Fatal(errRemove)
	}
	plain := devyrePersistService(expiringFirstRoutingConfig("expiring-first", nil, false), configPath)
	plain.devyreStartSessionPersistence()
	if plain.devyreSessions.persistence != nil {
		t.Fatal("a selector without session affinity persists")
	}
	plain.devyreStopSessionPersistence()
	if _, errStat := os.Stat(file); !os.IsNotExist(errStat) {
		t.Fatalf("a service without session affinity wrote %s: %v", file, errStat)
	}
}

// devyreAffinityConfig is an expiring-first config with session affinity.
func devyreAffinityConfig(gate float64, ttl string) *internalconfig.Config {
	cfg := expiringFirstRoutingConfig("expiring-first", &gate, false)
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinityTTL = ttl
	return cfg
}

// A routing change in a config reload rebuilds the selector; the bindings of
// the old one come along, whatever changed, the TTL included.
func TestDevyreSessionBindingsSurviveRoutingReload(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	apply := func(sequence uint64, cfg *internalconfig.Config) coreauth.Selector {
		t.Helper()
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: sequence}) {
			t.Fatalf("applyManagerConfig(#%d) failed", sequence)
		}
		return service.coreManager.Selector()
	}
	first := apply(1, devyreAffinityConfig(2, "1h"))
	auths := []*coreauth.Auth{{ID: "reload-auth-a", Provider: "claude"}, {ID: "reload-auth-b", Provider: "claude"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"reload-session"}}, Metadata: map[string]any{}}
	bound, err := first.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
	if err != nil {
		t.Fatal(err)
	}

	previous := first
	for index, cfg := range []*internalconfig.Config{
		devyreAffinityConfig(5, "1h"), // a gate change
		devyreAffinityConfig(5, "2h"), // a TTL change
	} {
		sequence := uint64(index + 2)
		selector := apply(sequence, cfg)
		if selector == previous {
			t.Fatalf("config #%d did not rebuild the selector", sequence)
		}
		affinity, ok := selector.(*coreauth.SessionAffinitySelector)
		if !ok {
			t.Fatalf("selector = %T", selector)
		}
		if authID, status := affinity.LookupAffinity("claude", "claude-opus-5-5", "claude:reload-session"); status != "bound" || authID != bound.ID {
			t.Fatalf("after config #%d the session is %s %q, want bound to %s", sequence, status, authID, bound.ID)
		}
		previous = selector
	}
}
