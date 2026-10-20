package cliproxy

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

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
