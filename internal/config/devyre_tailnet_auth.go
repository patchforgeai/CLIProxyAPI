package config

// devyre: management.tailnet-auth lets allowed tailnet devices, and optionally
// this PC, use the management API (and optionally the proxy API) without a key.
// Every setting defaults to off and every empty list fails closed. The trust
// decision lives in internal/tailnetauth; this file only holds the settings, so
// the fork diff stays isolated.

// TailnetAuthConfig configures passwordless access for requests that reach the
// server through tailscale serve or directly from this PC.
type TailnetAuthConfig struct {
	// Enabled is the master switch. Default: false.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// AllowedLogins lists the Tailscale-User-Login values allowed without a key.
	// An empty list disables all tailnet trust (fail closed).
	AllowedLogins []string `yaml:"allowed-logins" json:"allowed-logins"`

	// AllowedDevices lists the tailnet IPs (100.64.0.0/10 or fd7a:115c:a1e0::/48)
	// allowed without a key. An empty list means no tailnet device is keyless
	// (fail closed). This list is authoritative: every node of a single-owner
	// tailnet, automation hosts included, carries the owner's login, so it is the
	// only setting that keeps such hosts out.
	AllowedDevices []string `yaml:"allowed-devices" json:"allowed-devices"`

	// AllowedHosts lists the request Host names that keyless requests may target:
	// without port, case-insensitive, trailing dot ignored. An empty list
	// disables all keyless trust (fail closed).
	AllowedHosts []string `yaml:"allowed-hosts" json:"allowed-hosts"`

	// AllowLocal trusts direct requests from this PC: no X-Forwarded-For, no
	// Tailscale-* headers, and a loopback Host (localhost, 127.0.0.1, ::1) that is
	// also listed in AllowedHosts. Default: false.
	AllowLocal bool `yaml:"allow-local" json:"allow-local"`

	// ProxyAPI also accepts trusted requests on the proxy API without an API key.
	// Default: false.
	ProxyAPI bool `yaml:"proxy-api" json:"proxy-api"`
}
