package config

import (
	"math"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// devyre: settings for the expiring-first routing strategy and the quota
// observation that feeds it. They live under routing: next to the upstream
// strategy settings and are kept in this file so the fork diff stays isolated.

const (
	defaultExpiringFirstGatePercent = 2.0
	defaultClaudeUsageCacheTTL      = 5 * time.Minute
	defaultCodexUsageCacheTTL       = 60 * time.Second
	defaultUsageCacheRefreshFloor   = 30 * time.Second
	defaultQuotaPollClaudeInterval  = 30 * time.Minute
	defaultQuotaPollClaudeMinGap    = 10 * time.Minute
	defaultQuotaPollCodexInterval   = 5 * time.Minute
)

// ExpiringFirstConfig tunes the expiring-first credential selector.
type ExpiringFirstConfig struct {
	// GateRemainingPercent gates a credential when a short or matching
	// model-scoped window has at most this much quota left. Default: 2.
	GateRemainingPercent *float64 `yaml:"gate-remaining-percent,omitempty" json:"gate-remaining-percent,omitempty"`

	// LogPicks logs every pick at info level instead of debug.
	LogPicks bool `yaml:"log-picks,omitempty" json:"log-picks,omitempty"`
}

// GatePercent returns the gate threshold: 2 when unset or NaN, clamped to 0..100.
func (c ExpiringFirstConfig) GatePercent() float64 {
	if c.GateRemainingPercent == nil || math.IsNaN(*c.GateRemainingPercent) {
		return defaultExpiringFirstGatePercent
	}
	return math.Min(math.Max(*c.GateRemainingPercent, 0), 100)
}

// UsageCacheConfig tunes the management api-call cache for provider usage endpoints.
type UsageCacheConfig struct {
	// Enabled turns the cache on or off. Default: true, whatever the strategy.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// ClaudeUsageTTL is how long a Claude usage response is served from cache. Default: 5m.
	ClaudeUsageTTL string `yaml:"claude-usage-ttl,omitempty" json:"claude-usage-ttl,omitempty"`

	// CodexUsageTTL is how long a Codex usage response is served from cache. Default: 60s.
	CodexUsageTTL string `yaml:"codex-usage-ttl,omitempty" json:"codex-usage-ttl,omitempty"`

	// RefreshFloor is the minimum age of the last upstream call before an
	// explicit refresh request may bypass the cache. Default: 30s.
	RefreshFloor string `yaml:"refresh-floor,omitempty" json:"refresh-floor,omitempty"`
}

// IsEnabled reports whether the usage cache is on. Default: true.
func (c UsageCacheConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// ClaudeUsageTTLDuration returns the Claude usage TTL. Default: 5m.
func (c UsageCacheConfig) ClaudeUsageTTLDuration() time.Duration {
	return devyreParseDuration(c.ClaudeUsageTTL, defaultClaudeUsageCacheTTL)
}

// CodexUsageTTLDuration returns the Codex usage TTL. Default: 60s.
func (c UsageCacheConfig) CodexUsageTTLDuration() time.Duration {
	return devyreParseDuration(c.CodexUsageTTL, defaultCodexUsageCacheTTL)
}

// RefreshFloorDuration returns the cache bypass floor. Default: 30s.
func (c UsageCacheConfig) RefreshFloorDuration() time.Duration {
	return devyreParseDuration(c.RefreshFloor, defaultUsageCacheRefreshFloor)
}

// QuotaPollerConfig tunes the idle-credential usage poller.
type QuotaPollerConfig struct {
	// Enabled turns the poller on or off. When unset, it runs exactly when the
	// routing strategy is expiring-first; see RoutingConfig.QuotaPollerEnabled.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// ClaudeInterval is the age of the newest long-window reading after which a
	// Claude credential is polled. Default: 30m.
	ClaudeInterval string `yaml:"claude-interval,omitempty" json:"claude-interval,omitempty"`

	// ClaudeMinGap is the minimum gap between any two Claude usage calls. Default: 10m.
	ClaudeMinGap string `yaml:"claude-min-gap,omitempty" json:"claude-min-gap,omitempty"`

	// CodexInterval is the age of the newest long-window reading after which a
	// Codex credential is polled. Default: 5m.
	CodexInterval string `yaml:"codex-interval,omitempty" json:"codex-interval,omitempty"`
}

// ClaudeIntervalDuration returns the Claude poll interval. Default: 30m.
func (c QuotaPollerConfig) ClaudeIntervalDuration() time.Duration {
	return devyreParseDuration(c.ClaudeInterval, defaultQuotaPollClaudeInterval)
}

// ClaudeMinGapDuration returns the provider-wide Claude poll gap. Default: 10m.
func (c QuotaPollerConfig) ClaudeMinGapDuration() time.Duration {
	return devyreParseDuration(c.ClaudeMinGap, defaultQuotaPollClaudeMinGap)
}

// CodexIntervalDuration returns the Codex poll interval. Default: 5m.
func (c QuotaPollerConfig) CodexIntervalDuration() time.Duration {
	return devyreParseDuration(c.CodexInterval, defaultQuotaPollCodexInterval)
}

// QuotaObservationConfig groups the sources that feed quota readings.
type QuotaObservationConfig struct {
	UsageCache UsageCacheConfig  `yaml:"usage-cache,omitempty" json:"usage-cache,omitempty"`
	Poller     QuotaPollerConfig `yaml:"poller,omitempty" json:"poller,omitempty"`
}

// IsExpiringFirstStrategy reports whether strategy names the expiring-first
// selector: "expiring-first", "expiringfirst", "ef" or "soonest-reset",
// trimmed and case-insensitive.
func IsExpiringFirstStrategy(strategy string) bool {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "expiring-first", "expiringfirst", "ef", "soonest-reset":
		return true
	default:
		return false
	}
}

// QuotaPollerEnabled reports whether the idle-credential poller should run:
// the explicit poller.enabled when set, otherwise whether the strategy is
// expiring-first.
func (r RoutingConfig) QuotaPollerEnabled() bool {
	if r.QuotaObservation.Poller.Enabled != nil {
		return *r.QuotaObservation.Poller.Enabled
	}
	return IsExpiringFirstStrategy(r.Strategy)
}

// devyreParseDuration parses raw with time.ParseDuration. Empty, invalid or
// non-positive values fall back to def.
func devyreParseDuration(raw string, def time.Duration) time.Duration {
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return def
	}
	return parsed
}

// devyrePointerSettings are the pointer-backed routing settings whose explicit
// zero value (0 or false) differs from leaving them unset.
var devyrePointerSettings = []string{
	"routing.expiring-first.gate-remaining-percent",
	"routing.quota-observation.usage-cache.enabled",
	"routing.quota-observation.poller.enabled",
}

// devyreKeepsExplicitZero reports whether a node that a save is about to add
// must be written even though it looks like a zero default: it is, or it
// contains, an explicitly set pointer-backed routing setting. The marshaled
// config only carries these keys when the pointer is set.
func devyreKeepsExplicitZero(path []string, node *yaml.Node) bool {
	if node == nil || len(path) == 0 {
		return false
	}
	current := strings.Join(path, ".")
	for _, setting := range devyrePointerSettings {
		leaf := node
		if setting != current {
			rest, ok := strings.CutPrefix(setting, current+".")
			if !ok || node.Kind != yaml.MappingNode {
				continue
			}
			if leaf = yamlPath(node, rest); leaf == nil {
				continue
			}
		}
		if leaf.Kind == yaml.ScalarNode && leaf.Tag != "!!null" {
			return true
		}
	}
	return false
}
