package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func expiringFirstRoutingConfig(strategy string, gatePercent *float64, logPicks bool) *internalconfig.Config {
	return &internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy:      strategy,
		ExpiringFirst: internalconfig.ExpiringFirstConfig{GateRemainingPercent: gatePercent, LogPicks: logPicks},
	}}
}

func TestExpiringFirstRoutingStrategyAliases(t *testing.T) {
	for _, alias := range []string{"expiring-first", "expiringfirst", "ef", "soonest-reset", " Expiring-First ", "EF", "Soonest-Reset"} {
		state := normalizedRoutingRuntimeState(expiringFirstRoutingConfig(alias, nil, false))
		if state.strategy != "expiring-first" || state.expiringFirstGatePercent != 2 || state.expiringFirstLogPicks {
			t.Fatalf("normalizedRoutingRuntimeState(%q) = %+v, want expiring-first with the default gate", alias, state)
		}
		selector, ok := newRoutingSelector(state).(*coreauth.ExpiringFirstSelector)
		if !ok || selector.GateRemainingPercent != 2 || selector.LogPicks || selector.Now != nil {
			t.Fatalf("newRoutingSelector(%q) = %#v, want *auth.ExpiringFirstSelector with gate 2", alias, newRoutingSelector(state))
		}
	}
}

func TestExpiringFirstRoutingSettings(t *testing.T) {
	zero, five := 0.0, 5.0
	state := normalizedRoutingRuntimeState(expiringFirstRoutingConfig("expiring-first", &zero, true))
	selector, ok := newRoutingSelector(state).(*coreauth.ExpiringFirstSelector)
	if !ok || selector.GateRemainingPercent != 0 || !selector.LogPicks {
		t.Fatalf("selector = %#v, want an explicit gate of 0 and log-picks", newRoutingSelector(state))
	}

	cfg := expiringFirstRoutingConfig("ef", &five, false)
	cfg.Routing.SessionAffinity = true
	if _, ok := newRoutingSelector(normalizedRoutingRuntimeState(cfg)).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector with session affinity = %T, want *auth.SessionAffinitySelector", newRoutingSelector(normalizedRoutingRuntimeState(cfg)))
	}

	// The settings only matter under expiring-first, so editing them under
	// another strategy must not change the state and rebuild the selector.
	plain := normalizedRoutingRuntimeState(expiringFirstRoutingConfig("round-robin", nil, false))
	tuned := normalizedRoutingRuntimeState(expiringFirstRoutingConfig("round-robin", &five, true))
	if plain != tuned {
		t.Fatalf("round-robin states differ by expiring-first settings: %+v vs %+v", plain, tuned)
	}
	if _, ok := newRoutingSelector(tuned).(*coreauth.RoundRobinSelector); !ok {
		t.Fatalf("round-robin selector = %T", newRoutingSelector(tuned))
	}
}

// The strategy and its settings hot-reload through the regular config apply path.
func TestExpiringFirstRoutingHotReload(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	apply := func(sequence uint64, cfg *internalconfig.Config) coreauth.Selector {
		t.Helper()
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: sequence}) {
			t.Fatalf("applyManagerConfig(#%d) failed", sequence)
		}
		return service.coreManager.Selector()
	}

	if _, ok := apply(1, expiringFirstRoutingConfig("round-robin", nil, false)).(*coreauth.RoundRobinSelector); !ok {
		t.Fatalf("initial selector = %T, want round-robin", service.coreManager.Selector())
	}
	first, ok := apply(2, expiringFirstRoutingConfig("soonest-reset", nil, false)).(*coreauth.ExpiringFirstSelector)
	if !ok || first.GateRemainingPercent != 2 {
		t.Fatalf("selector after switching strategy = %#v, want expiring-first", service.coreManager.Selector())
	}
	if again := apply(3, expiringFirstRoutingConfig("expiring-first", nil, false)); again != coreauth.Selector(first) {
		t.Fatal("an unchanged expiring-first config rebuilt the selector and lost its rotation state")
	}
	five := 5.0
	tuned, ok := apply(4, expiringFirstRoutingConfig("expiring-first", &five, true)).(*coreauth.ExpiringFirstSelector)
	if !ok || tuned == first || tuned.GateRemainingPercent != 5 || !tuned.LogPicks {
		t.Fatalf("selector after tuning = %#v, want a new selector with gate 5 and log-picks", service.coreManager.Selector())
	}
}
