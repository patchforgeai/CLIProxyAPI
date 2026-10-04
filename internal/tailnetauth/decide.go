// Package tailnetauth decides whether a request may use the management API, and
// optionally the proxy API, without a key because it comes from an allowed
// tailnet device through tailscale serve, or directly from this PC.
//
// devyre: fork-only package. Decide is a pure function of the policy and the
// request, so every rule is unit-tested in isolation. The HTTP layers that apply
// it live in the devyre_tailnet_auth.go files next to the management handler and
// the API server.
//
// Facts the rules rely on (tailscale serve 1.102.4, ipn/ipnlocal/serve.go):
//   - serve proxies to http://127.0.0.1:8317, overwrites X-Forwarded-For with the
//     single tailnet source IP, sets X-Forwarded-Host to the incoming Host, keeps
//     Host, deletes every client-supplied Tailscale-User-*, Tailscale-Funnel-Request
//     and Tailscale-Headers-Info header, then stamps Tailscale-User-Login and
//     Tailscale-User-Name (RFC 2047 Q-encoded when non-ASCII) for untagged,
//     user-owned nodes. Tagged nodes get no identity headers; Funnel requests get
//     Tailscale-Funnel-Request.
//   - serve only forwards requests whose Host is this machine's tailnet name.
//   - Inside the container every request, from serve, from a browser on this PC or
//     from another container, arrives from the Docker bridge gateway. Anything that
//     reaches 127.0.0.1:8317 can therefore forge every header above; the decision
//     narrows that (loopback Hosts never accept proxy headers) but cannot remove it.
//   - Browsers send no Sec-Fetch-* headers to URLs that are not potentially
//     trustworthy, such as the plain-HTTP tailnet URL, and no Origin on GET/HEAD
//     no-cors requests (navigation, img, script, iframe, redirects). Requiring an
//     affirmative non-browser signal when Origin is absent (rule R5b) is what keeps
//     cross-site pages out on that path; the Sec-Fetch-Site check is defense in depth.
package tailnetauth

import (
	"mime"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Authentication methods. A trusted Decision reports MethodTailnet or
// MethodLocal; the session endpoint reports MethodKey for key-authenticated
// requests.
const (
	MethodTailnet = "tailnet"
	MethodLocal   = "local"
	MethodKey     = "key"
)

// KeylessHeader is the constant, non-secret header that the panel and scripts
// send to show the request is not a browser navigation or subresource load. Its
// only accepted value is "1".
const KeylessHeader = "X-CPA-Keyless"

var (
	tailnetIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// singleValueHeaders may appear at most once; a repeated header is ambiguous.
var singleValueHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"Origin",
	"Sec-Fetch-Site",
	KeylessHeader,
	"Tailscale-User-Login",
	"Tailscale-User-Name",
}

// proxyHeaders mark a request that passed through a proxy. tailscale serve only
// forwards tailnet Host names, so a loopback Host carrying one of them is forged.
var proxyHeaders = []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"}

// signalKeyHeaders count as a non-browser signal when they carry any non-empty
// value. Browsers never add them on their own.
var signalKeyHeaders = []string{"X-Management-Key", "X-Api-Key", "X-Goog-Api-Key"}

// Policy is management.tailnet-auth combined with server.trusted-proxies.
type Policy struct {
	Enabled        bool
	AllowedLogins  []string
	AllowedDevices []string
	AllowedHosts   []string
	AllowLocal     bool
	TrustedProxies []string
}

// PolicyFromConfig builds the policy from cfg. A nil cfg yields a disabled policy.
// The returned slices alias cfg's, which are replaced, never edited in place.
func PolicyFromConfig(cfg *config.Config) Policy {
	if cfg == nil {
		return Policy{}
	}
	settings := cfg.RemoteManagement.TailnetAuth
	return Policy{
		Enabled:        settings.Enabled,
		AllowedLogins:  settings.AllowedLogins,
		AllowedDevices: settings.AllowedDevices,
		AllowedHosts:   settings.AllowedHosts,
		AllowLocal:     settings.AllowLocal,
		TrustedProxies: cfg.TrustedProxies,
	}
}

// Request holds the parts of an HTTP request the decision reads.
type Request struct {
	// PeerIP is the direct TCP peer (gin's c.RemoteIP()), never a forwarded address.
	PeerIP string
	// Host is the request Host (http.Request.Host).
	Host string
	// Header holds the request headers.
	Header http.Header
}

// FromHTTP builds a Request from r and its direct TCP peer IP.
func FromHTTP(peerIP string, r *http.Request) Request {
	if r == nil {
		return Request{PeerIP: peerIP}
	}
	return Request{PeerIP: peerIP, Host: r.Host, Header: r.Header}
}

// Decision is the outcome of Decide. Login and Device are set only for a trusted
// tailnet request; Login is empty for an explicitly listed tagged node.
type Decision struct {
	Trusted bool
	Method  string
	Login   string
	Device  string
	Reason  string
}

// Principal is the stable proxy API principal of a trusted request:
// "tailnet:<login>@<device IP>" or "local". It is empty when not trusted.
func (d Decision) Principal() string {
	switch {
	case !d.Trusted:
		return ""
	case d.Method == MethodLocal:
		return "local"
	default:
		return "tailnet:" + d.Login + "@" + d.Device
	}
}

func deny(reason string) Decision {
	return Decision{Reason: reason}
}

// Decide applies the tailnet-auth rules to r. Anything not explicitly trusted is
// untrusted, and the caller then runs its normal key check.
func Decide(p Policy, r Request) Decision {
	// R1: master switch.
	if !p.Enabled {
		return deny("tailnet-auth disabled")
	}
	// R2: only loopback and configured proxies may relay requests.
	peer, ok := parseAddr(r.PeerIP)
	if !ok || !(peer.IsLoopback() || inTrustedProxies(peer, p.TrustedProxies)) {
		return deny("direct peer is neither loopback nor a trusted proxy")
	}
	header := r.Header
	if header == nil {
		header = http.Header{}
	}
	// AM8: a repeated header is ambiguous.
	for _, name := range singleValueHeaders {
		if len(header.Values(name)) > 1 {
			return deny("duplicate " + name)
		}
	}
	// R3: Funnel traffic comes from the internet.
	if len(header.Values("Tailscale-Funnel-Request")) > 0 {
		return deny("Tailscale Funnel request")
	}
	// R4: the Host, and the X-Forwarded-Host serve copies from it, must be allowed.
	host, ok := parseHostPort(r.Host)
	if !ok {
		return deny("missing or invalid Host")
	}
	if !hostAllowed(host.name, p.AllowedHosts) {
		return deny("Host not in allowed-hosts")
	}
	if values := header.Values("X-Forwarded-Host"); len(values) == 1 {
		forwarded, okForwarded := parseHostPort(values[0])
		if !okForwarded || forwarded != host {
			return deny("X-Forwarded-Host does not match Host")
		}
	}
	// R5 and R5b: keep browsers on other sites out.
	if reason := browserGuard(header, host); reason != "" {
		return deny(reason)
	}
	// AM4: the Host class picks the only path that can apply.
	if isLoopbackHost(host.name) {
		return decideLocal(p, header)
	}
	return decideTailnet(p, header)
}

// decideLocal is rule R7 for a loopback Host.
func decideLocal(p Policy, header http.Header) Decision {
	for _, name := range proxyHeaders {
		if len(header.Values(name)) > 0 {
			return deny(textproto.CanonicalMIMEHeaderKey(name) + " on a loopback Host")
		}
	}
	for name := range header {
		if strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(name), "Tailscale-") {
			return deny("Tailscale header on a loopback Host")
		}
	}
	if !p.AllowLocal {
		return deny("allow-local is off")
	}
	return Decision{Trusted: true, Method: MethodLocal, Reason: "direct request from this PC"}
}

// decideTailnet is rule R6 for a tailnet Host.
//
// allowed-devices is authoritative. Every node on a single-owner tailnet, the CI
// runner and bot VMs included, is untagged and owned by the owner's login, so
// serve stamps all of them with the allowed login. Only the device list tells
// them apart, which is why an empty list trusts no device at all.
func decideTailnet(p Policy, header http.Header) Decision {
	values := header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return deny("tailnet Host without X-Forwarded-For")
	}
	raw := strings.TrimSpace(values[0])
	if strings.Contains(raw, ",") {
		return deny("X-Forwarded-For lists more than one address")
	}
	device, ok := parseAddr(raw)
	if !ok {
		return deny("invalid X-Forwarded-For")
	}
	if !tailnetIPv4.Contains(device) && !tailnetIPv6.Contains(device) {
		return deny("X-Forwarded-For is not a tailnet address")
	}
	if !hasEntry(p.AllowedLogins) {
		return deny("allowed-logins is empty")
	}
	if !deviceAllowed(device, p.AllowedDevices) {
		return deny("device not in allowed-devices")
	}
	login := ""
	if hasIdentityHeader(header) {
		logins := header.Values("Tailscale-User-Login")
		if len(logins) == 0 {
			return deny("Tailscale-User-* headers without Tailscale-User-Login")
		}
		decoded, okLogin := decodeLogin(logins[0])
		if !okLogin {
			return deny("invalid Tailscale-User-Login")
		}
		if !loginAllowed(decoded, p.AllowedLogins) {
			return deny("login not in allowed-logins")
		}
		login = decoded
	}
	return Decision{Trusted: true, Method: MethodTailnet, Login: login, Device: device.String(), Reason: "allowed tailnet device"}
}

// browserGuard implements R5 and R5b. It returns a denial reason, or "".
//
// R5 is defense in depth: the plain-HTTP tailnet path never carries Sec-Fetch-*
// headers, and same-origin GETs carry no Origin. R5b closes that gap: without
// Origin, the request must carry something a browser only sends when a script
// asked for it. A cross-origin page can attach such a header only in CORS mode,
// which always adds Origin, and R5 then rejects that Origin even though the CORS
// middleware approves every preflight.
func browserGuard(header http.Header, host hostPort) string {
	if values := header.Values("Sec-Fetch-Site"); len(values) == 1 {
		switch strings.ToLower(strings.TrimSpace(values[0])) {
		case "same-origin", "none":
		default:
			return "Sec-Fetch-Site is not same-origin or none"
		}
	}
	origins := header.Values("Origin")
	if len(origins) == 0 {
		if !hasNonBrowserSignal(header) {
			return "no Origin and no non-browser signal"
		}
		return ""
	}
	origin := strings.TrimSpace(origins[0])
	if origin == "" || strings.EqualFold(origin, "null") {
		return "opaque Origin"
	}
	scheme, originHost, ok := parseOrigin(origin)
	if !ok {
		return "invalid Origin"
	}
	if originHost.name != host.name || effectivePort(originHost.port, scheme) != effectivePort(host.port, scheme) {
		return "Origin does not match Host"
	}
	return ""
}

// hasNonBrowserSignal reports whether the request carries X-CPA-Keyless: 1, a
// non-empty Bearer token, or a non-empty key header. Query keys, other
// Authorization schemes and headers that browsers add on their own never count.
func hasNonBrowserSignal(header http.Header) bool {
	if values := header.Values(KeylessHeader); len(values) == 1 && values[0] == "1" {
		return true
	}
	if scheme, token, found := strings.Cut(strings.TrimSpace(header.Get("Authorization")), " "); found &&
		strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != "" {
		return true
	}
	for _, name := range signalKeyHeaders {
		if strings.TrimSpace(header.Get(name)) != "" {
			return true
		}
	}
	return false
}

func hasIdentityHeader(header http.Header) bool {
	for name := range header {
		if strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(name), "Tailscale-User-") {
			return true
		}
	}
	return false
}

// decodeLogin decodes an RFC 2047 encoded Tailscale-User-Login value.
func decodeLogin(raw string) (string, bool) {
	var decoder mime.WordDecoder
	decoded, err := decoder.DecodeHeader(strings.TrimSpace(raw))
	decoded = strings.TrimSpace(decoded)
	if err != nil || decoded == "" {
		return "", false
	}
	return decoded, true
}

func loginAllowed(login string, allowed []string) bool {
	for _, entry := range allowed {
		if entry = strings.TrimSpace(entry); entry != "" && strings.EqualFold(entry, login) {
			return true
		}
	}
	return false
}

func hasEntry(list []string) bool {
	for _, entry := range list {
		if strings.TrimSpace(entry) != "" {
			return true
		}
	}
	return false
}

func deviceAllowed(device netip.Addr, allowed []string) bool {
	for _, entry := range allowed {
		if addr, ok := parseAddr(entry); ok && addr == device {
			return true
		}
	}
	return false
}

// parseAddr parses a bare IP address without zone and unmaps IPv4-mapped IPv6.
func parseAddr(raw string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// inTrustedProxies reports whether peer matches an entry of server.trusted-proxies:
// an IP or a CIDR. Entries that do not parse are ignored.
func inTrustedProxies(peer netip.Addr, entries []string) bool {
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			if addr, ok := parseAddr(entry); ok && addr == peer {
				return true
			}
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			continue
		}
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Masked().Contains(peer) {
			return true
		}
	}
	return false
}

// hostPort is a normalized Host: lowercase, no trailing dot, no IPv6 brackets,
// canonical IP literals, and port 0 when absent.
type hostPort struct {
	name string
	port int
}

// parseHostPort normalizes a Host header value or the authority of an Origin.
func parseHostPort(raw string) (hostPort, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return hostPort{}, false
	}
	name, portText := raw, ""
	hasPort := false
	if strings.HasPrefix(raw, "[") {
		end := strings.IndexByte(raw, ']')
		if end < 0 {
			return hostPort{}, false
		}
		name = raw[1:end]
		if rest := raw[end+1:]; rest != "" {
			if rest[0] != ':' {
				return hostPort{}, false
			}
			portText, hasPort = rest[1:], true
		}
		addr, err := netip.ParseAddr(name)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return hostPort{}, false
		}
		name = addr.String()
	} else {
		if before, after, found := strings.Cut(raw, ":"); found {
			if strings.Contains(after, ":") {
				return hostPort{}, false // an IPv6 literal needs brackets
			}
			name, portText, hasPort = before, after, true
		}
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if !validHostName(name) {
			return hostPort{}, false
		}
		if addr, err := netip.ParseAddr(name); err == nil {
			name = addr.String()
		}
	}
	port := 0
	if hasPort {
		parsed, ok := parsePort(portText)
		if !ok {
			return hostPort{}, false
		}
		port = parsed
	}
	return hostPort{name: name, port: port}, true
}

func parsePort(text string) (int, bool) {
	if text == "" || len(text) > 5 {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// validHostName accepts DNS-style names and IPv4 literals made of letters,
// digits, hyphens, underscores and dots, with no empty labels.
func validHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return false
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return false
			}
		}
	}
	return true
}

// normalizeAllowedHost turns an allowed-hosts entry into a host name; a port in
// the entry is ignored. It returns "" for an entry that does not parse.
func normalizeAllowedHost(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ""
	}
	// A bare IP literal, IPv6 included, is written without brackets. It keeps
	// the form parseHostPort gives a Host (IPv4-mapped IPv6 is not unmapped).
	if addr, err := netip.ParseAddr(entry); err == nil {
		if addr.Zone() != "" {
			return ""
		}
		return addr.String()
	}
	if host, ok := parseHostPort(entry); ok {
		return host.name
	}
	return ""
}

func hostAllowed(name string, allowed []string) bool {
	for _, entry := range allowed {
		if normalized := normalizeAllowedHost(entry); normalized != "" && normalized == name {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether a normalized Host name is a loopback name.
func isLoopbackHost(name string) bool {
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(name)
	return err == nil && addr.Unmap().IsLoopback()
}

// parseOrigin parses a serialized http or https origin: scheme://host[:port]
// with no path, query, fragment or user info.
func parseOrigin(raw string) (string, hostPort, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", hostPort{}, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", hostPort{}, false
	}
	host, ok := parseHostPort(parsed.Host)
	if !ok {
		return "", hostPort{}, false
	}
	return scheme, host, true
}

// effectivePort applies the scheme's default port when the port is absent.
func effectivePort(port int, scheme string) int {
	if port != 0 {
		return port
	}
	if scheme == "https" {
		return 443
	}
	return 80
}
