package tailnetauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The values below are fake: a CGNAT tailnet range, an example.com login and an
// example tailnet name. Real logins, IPs and hostnames never belong in the repo.
const (
	testLogin      = "owner@example.com"
	testDevice     = "100.64.0.10"
	testDeviceV6   = "fd7a:115c:a1e0::10"
	testUnlisted   = "100.64.0.99"
	testShortHost  = "devbox"
	testTailnetFQD = "devbox.example-tailnet.ts.net"
	testTailnetURL = testTailnetFQD + ":8318"
	testGateway    = "172.19.0.1" // Docker bridge gateway, inside 172.16.0.0/12
)

func testPolicy() Policy {
	return Policy{
		Enabled:        true,
		AllowedLogins:  []string{testLogin},
		AllowedDevices: []string{testDevice, testDeviceV6},
		AllowedHosts:   []string{testShortHost, testTailnetFQD, "localhost", "127.0.0.1"},
		AllowLocal:     true,
		TrustedProxies: []string{"127.0.0.1", "172.16.0.0/12", "192.168.65.0/24"},
	}
}

// tailnetRequest is a request exactly as tailscale serve forwards it from an
// allowed, untagged device of the owner, sent by a script with X-CPA-Keyless.
func tailnetRequest() Request {
	header := http.Header{}
	header.Set("X-Forwarded-For", testDevice)
	header.Set("X-Forwarded-Host", testTailnetURL)
	header.Set("Tailscale-User-Login", testLogin)
	header.Set("Tailscale-User-Name", "Owner")
	header.Set(KeylessHeader, "1")
	return Request{PeerIP: testGateway, Host: testTailnetURL, Header: header}
}

// localRequest is a direct request from this PC to the published loopback port.
func localRequest() Request {
	header := http.Header{}
	header.Set(KeylessHeader, "1")
	return Request{PeerIP: testGateway, Host: "127.0.0.1:8317", Header: header}
}

type decideCase struct {
	name    string
	base    func() Request
	policy  func(*Policy)
	request func(*Request)
	trusted bool
	method  string
	login   string
	device  string
	reason  string // substring of the denial reason
}

func runDecideCases(t *testing.T, cases []decideCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := testPolicy()
			if tc.policy != nil {
				tc.policy(&policy)
			}
			base := tc.base
			if base == nil {
				base = tailnetRequest
			}
			request := base()
			if tc.request != nil {
				tc.request(&request)
			}
			got := Decide(policy, request)
			if got.Trusted != tc.trusted {
				t.Fatalf("Trusted = %v, want %v (reason %q)", got.Trusted, tc.trusted, got.Reason)
			}
			if tc.trusted {
				if got.Method != tc.method || got.Login != tc.login || got.Device != tc.device {
					t.Fatalf("decision = %+v, want method %q login %q device %q", got, tc.method, tc.login, tc.device)
				}
				return
			}
			if got.Method != "" || got.Login != "" || got.Device != "" {
				t.Fatalf("untrusted decision leaks identity: %+v", got)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("Reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
		})
	}
}

func setHeader(name, value string) func(*Request) {
	return func(r *Request) { r.Header.Set(name, value) }
}

func addHeader(name, value string) func(*Request) {
	return func(r *Request) { r.Header.Add(name, value) }
}

func delHeader(names ...string) func(*Request) {
	return func(r *Request) {
		for _, name := range names {
			r.Header.Del(name)
		}
	}
}

func chain(steps ...func(*Request)) func(*Request) {
	return func(r *Request) {
		for _, step := range steps {
			step(r)
		}
	}
}

func setHost(host string) func(*Request) {
	return func(r *Request) {
		r.Host = host
		if r.Header.Get("X-Forwarded-Host") != "" {
			r.Header.Set("X-Forwarded-Host", host)
		}
	}
}

func TestDecide_BaseRequestsAreTrusted(t *testing.T) {
	runDecideCases(t, []decideCase{
		{name: "tailnet device of the owner", trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "local request", base: localRequest, trusted: true, method: MethodLocal},
	})
}

func TestDecide_R1_MasterSwitch(t *testing.T) {
	runDecideCases(t, []decideCase{
		{name: "disabled tailnet", policy: func(p *Policy) { p.Enabled = false }, reason: "disabled"},
		{name: "disabled local", base: localRequest, policy: func(p *Policy) { p.Enabled = false }, reason: "disabled"},
		{name: "zero policy", policy: func(p *Policy) { *p = Policy{} }, reason: "disabled"},
	})
}

func TestDecide_R2_DirectPeer(t *testing.T) {
	peer := func(ip string) func(*Request) { return func(r *Request) { r.PeerIP = ip } }
	runDecideCases(t, []decideCase{
		{name: "loopback peer", request: peer("127.0.0.1"), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "other loopback address", request: peer("127.0.0.2"), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "IPv6 loopback peer", request: peer("::1"), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "IPv4-mapped trusted peer", request: peer("::ffff:172.19.0.1"), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "Docker Desktop VM network", request: peer("192.168.65.1"), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "LAN peer", request: peer("192.168.1.20"), reason: "trusted proxy"},
		{name: "tailnet peer connecting directly", request: peer(testDevice), reason: "trusted proxy"},
		{name: "public peer", request: peer("203.0.113.7"), reason: "trusted proxy"},
		{name: "empty peer", request: peer(""), reason: "trusted proxy"},
		{name: "peer with port", request: peer("172.19.0.1:1234"), reason: "trusted proxy"},
		{name: "peer with zone", request: peer("fe80::1%eth0"), reason: "trusted proxy"},
		{name: "no trusted proxies", policy: func(p *Policy) { p.TrustedProxies = nil }, reason: "trusted proxy"},
		{name: "invalid trusted proxy entries are ignored", policy: func(p *Policy) { p.TrustedProxies = []string{"not-an-ip", "172.16.0.0/99", " "} }, reason: "trusted proxy"},
		{name: "single IP entry", policy: func(p *Policy) { p.TrustedProxies = []string{testGateway} }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "unmasked CIDR entry", policy: func(p *Policy) { p.TrustedProxies = []string{"172.19.0.9/16"} }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "IPv4-mapped CIDR entry", policy: func(p *Policy) { p.TrustedProxies = []string{"::ffff:172.16.0.0/108"} }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "local request from loopback peer", base: localRequest, request: peer("127.0.0.1"), trusted: true, method: MethodLocal},
		{name: "local request from LAN peer", base: localRequest, request: peer("192.168.1.20"), reason: "trusted proxy"},
	})
}

func TestDecide_AM8_DuplicateHeaders(t *testing.T) {
	cases := []decideCase{}
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "Tailscale-User-Login", "Tailscale-User-Name", KeylessHeader} {
		values := tailnetRequest().Header.Values(name)
		cases = append(cases, decideCase{name: "duplicate " + name, request: addHeader(name, values[0]), reason: "duplicate " + name})
	}
	origin := "http://" + testTailnetURL
	cases = append(cases,
		decideCase{name: "duplicate Origin", request: chain(addHeader("Origin", origin), addHeader("Origin", origin)), reason: "duplicate Origin"},
		decideCase{name: "duplicate Sec-Fetch-Site", request: chain(addHeader("Sec-Fetch-Site", "same-origin"), addHeader("Sec-Fetch-Site", "same-origin")), reason: "duplicate Sec-Fetch-Site"},
		decideCase{name: "duplicate X-Forwarded-For with a second address", request: addHeader("X-Forwarded-For", testUnlisted), reason: "duplicate X-Forwarded-For"},
		decideCase{name: "duplicate login on local path", base: localRequest, request: chain(addHeader("Tailscale-User-Login", testLogin), addHeader("Tailscale-User-Login", testLogin)), reason: "duplicate Tailscale-User-Login"},
	)
	runDecideCases(t, cases)
}

func TestDecide_R3_Funnel(t *testing.T) {
	runDecideCases(t, []decideCase{
		{name: "funnel marker", request: setHeader("Tailscale-Funnel-Request", "?1"), reason: "Funnel"},
		{name: "funnel marker with any value", request: setHeader("Tailscale-Funnel-Request", ""), reason: "Funnel"},
		{name: "funnel without identity", request: chain(delHeader("Tailscale-User-Login", "Tailscale-User-Name"), setHeader("Tailscale-Funnel-Request", "?1")), reason: "Funnel"},
		{name: "funnel marker on local path", base: localRequest, request: setHeader("Tailscale-Funnel-Request", "?1"), reason: "Funnel"},
	})
}

func TestDecide_R4_HostAndForwardedHost(t *testing.T) {
	trustedTailnet := func(name string, step func(*Request)) decideCase {
		return decideCase{name: name, request: step, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice}
	}
	runDecideCases(t, []decideCase{
		trustedTailnet("short name", setHost(testShortHost+":8318")),
		trustedTailnet("FQDN without port", setHost(testTailnetFQD)),
		trustedTailnet("upper case", setHost("DEVBOX.Example-Tailnet.TS.NET:8318")),
		trustedTailnet("trailing dot", setHost(testTailnetFQD+".:8318")),
		trustedTailnet("forwarded host normalized like Host", setHeader("X-Forwarded-Host", "DevBox.Example-Tailnet.ts.net.:8318")),
		trustedTailnet("no forwarded host", delHeader("X-Forwarded-Host")),
		{name: "host not allowed", request: setHost("other.example-tailnet.ts.net:8318"), reason: "allowed-hosts"},
		{name: "IP literal host", request: setHost(testDevice + ":8318"), reason: "allowed-hosts"},
		{name: "empty allowed-hosts", policy: func(p *Policy) { p.AllowedHosts = nil }, reason: "allowed-hosts"},
		{name: "empty allowed-hosts on local path", base: localRequest, policy: func(p *Policy) { p.AllowedHosts = nil }, reason: "allowed-hosts"},
		{name: "missing host", request: setHost(""), reason: "invalid Host"},
		{name: "empty port", request: setHost(testTailnetFQD + ":"), reason: "invalid Host"},
		{name: "port zero", request: setHost(testTailnetFQD + ":0"), reason: "invalid Host"},
		{name: "port out of range", request: setHost(testTailnetFQD + ":65536"), reason: "invalid Host"},
		{name: "signed port", request: setHost(testTailnetFQD + ":+8318"), reason: "invalid Host"},
		{name: "user info", request: setHost("evil@" + testTailnetFQD), reason: "invalid Host"},
		{name: "path in host", request: setHost(testTailnetFQD + "/x"), reason: "invalid Host"},
		{name: "empty label", request: setHost("devbox..example-tailnet.ts.net"), reason: "invalid Host"},
		{name: "bare IPv6", request: setHost("fd7a:115c:a1e0::1"), reason: "invalid Host"},
		{name: "forwarded host differs", request: setHeader("X-Forwarded-Host", "other.example-tailnet.ts.net:8318"), reason: "X-Forwarded-Host"},
		{name: "forwarded host differs in port", request: setHeader("X-Forwarded-Host", testTailnetFQD+":443"), reason: "X-Forwarded-Host"},
		{name: "forwarded host drops port", request: setHeader("X-Forwarded-Host", testTailnetFQD), reason: "X-Forwarded-Host"},
		{name: "empty forwarded host", request: setHeader("X-Forwarded-Host", ""), reason: "X-Forwarded-Host"},
		{name: "allowed-hosts entry with port matches", policy: func(p *Policy) { p.AllowedHosts = []string{testTailnetFQD + ":8318"} }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "allowed-hosts entry normalized", policy: func(p *Policy) { p.AllowedHosts = []string{" DEVBOX.example-tailnet.ts.net. "} }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
	})
}

func TestDecide_R4_IPv6Hosts(t *testing.T) {
	local := func(host string, entries ...string) decideCase {
		return decideCase{
			name:    "local " + host,
			base:    localRequest,
			policy:  func(p *Policy) { p.AllowedHosts = entries },
			request: setHost(host),
			trusted: true,
			method:  MethodLocal,
		}
	}
	runDecideCases(t, []decideCase{
		local("[::1]:8317", "::1"),
		local("[::1]", "::1"),
		local("[0:0:0:0:0:0:0:1]:8317", "::1"),
		local("[::1]:8317", "[::1]"),
		{name: "IPv6 host not allowed", base: localRequest, request: setHost("[::1]:8317"), reason: "allowed-hosts"},
		{name: "IPv6 host with zone", base: localRequest, policy: func(p *Policy) { p.AllowedHosts = []string{"fe80::1"} }, request: setHost("[fe80::1%25eth0]:8317"), reason: "invalid Host"},
		{name: "unterminated bracket", base: localRequest, request: setHost("[::1:8317"), reason: "invalid Host"},
		{name: "garbage after bracket", base: localRequest, request: setHost("[::1]x"), reason: "invalid Host"},
	})
}

func TestDecide_R5_BrowserGuard(t *testing.T) {
	trustedTailnet := func(name string, step func(*Request)) decideCase {
		return decideCase{name: name, request: step, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice}
	}
	runDecideCases(t, []decideCase{
		trustedTailnet("Sec-Fetch-Site same-origin", setHeader("Sec-Fetch-Site", "same-origin")),
		trustedTailnet("Sec-Fetch-Site none", setHeader("Sec-Fetch-Site", "none")),
		trustedTailnet("same origin", setHeader("Origin", "http://"+testTailnetURL)),
		trustedTailnet("same origin, upper case", setHeader("Origin", "http://DEVBOX.example-tailnet.ts.net:8318")),
		trustedTailnet("same origin without signal", chain(delHeader(KeylessHeader), setHeader("Origin", "http://"+testTailnetURL))),
		trustedTailnet("https origin, default port", chain(setHost(testTailnetFQD), setHeader("Origin", "https://"+testTailnetFQD))),
		trustedTailnet("https origin, explicit default port", chain(setHost(testTailnetFQD), setHeader("Origin", "https://"+testTailnetFQD+":443"))),
		trustedTailnet("http origin, explicit default port", chain(setHost(testTailnetFQD), setHeader("Origin", "http://"+testTailnetFQD+":80"))),
		trustedTailnet("host with default port", chain(setHost(testTailnetFQD+":80"), setHeader("Origin", "http://"+testTailnetFQD))),
		{name: "Sec-Fetch-Site cross-site", request: setHeader("Sec-Fetch-Site", "cross-site"), reason: "Sec-Fetch-Site"},
		{name: "Sec-Fetch-Site same-site", request: setHeader("Sec-Fetch-Site", "same-site"), reason: "Sec-Fetch-Site"},
		{name: "Sec-Fetch-Site empty", request: setHeader("Sec-Fetch-Site", ""), reason: "Sec-Fetch-Site"},
		{name: "Sec-Fetch-Site cross-site with keyless header", request: chain(setHeader("Sec-Fetch-Site", "cross-site"), setHeader(KeylessHeader, "1")), reason: "Sec-Fetch-Site"},
		{name: "cross-origin with keyless header", request: setHeader("Origin", "http://evil.example"), reason: "Origin does not match"},
		{name: "cross-origin with bearer", request: chain(setHeader("Origin", "http://evil.example"), setHeader("Authorization", "Bearer anything")), reason: "Origin does not match"},
		{name: "null origin", request: setHeader("Origin", "null"), reason: "opaque Origin"},
		{name: "empty origin", request: setHeader("Origin", ""), reason: "opaque Origin"},
		{name: "origin port differs", request: setHeader("Origin", "http://"+testTailnetFQD+":8319"), reason: "Origin does not match"},
		{name: "origin scheme default differs", request: chain(setHost(testTailnetFQD), setHeader("Origin", "https://"+testTailnetFQD+":80")), reason: "Origin does not match"},
		{name: "https origin against port 80 host", request: chain(setHost(testTailnetFQD+":80"), setHeader("Origin", "https://"+testTailnetFQD)), reason: "Origin does not match"},
		{name: "short-name origin against FQDN host", request: setHeader("Origin", "http://"+testShortHost+":8318"), reason: "Origin does not match"},
		{name: "origin with path", request: setHeader("Origin", "http://"+testTailnetURL+"/"), reason: "invalid Origin"},
		{name: "origin with query", request: setHeader("Origin", "http://"+testTailnetURL+"?x=1"), reason: "invalid Origin"},
		{name: "origin with user info", request: setHeader("Origin", "http://user@"+testTailnetURL), reason: "invalid Origin"},
		{name: "file origin", request: setHeader("Origin", "file://"), reason: "invalid Origin"},
		{name: "extension origin", request: setHeader("Origin", "chrome-extension://abcdef"), reason: "invalid Origin"},
		{name: "local cross-site fetch", base: localRequest, request: chain(setHeader("Sec-Fetch-Site", "cross-site"), setHeader("Origin", "http://evil.example")), reason: "Sec-Fetch-Site"},
		{name: "local cross-origin", base: localRequest, request: setHeader("Origin", "http://evil.example"), reason: "Origin does not match"},
		{name: "local origin localhost against 127.0.0.1 host", base: localRequest, request: setHeader("Origin", "http://localhost:8317"), reason: "Origin does not match"},
		{name: "local same origin", base: localRequest, request: chain(setHeader("Origin", "http://127.0.0.1:8317"), setHeader("Sec-Fetch-Site", "same-origin")), trusted: true, method: MethodLocal},
	})
}

func TestDecide_R5b_NonBrowserSignal(t *testing.T) {
	noSignal := delHeader(KeylessHeader)
	trustedTailnet := func(name string, step func(*Request)) decideCase {
		return decideCase{name: name, request: chain(noSignal, step), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice}
	}
	trustedLocal := func(name string, step func(*Request)) decideCase {
		return decideCase{name: name, base: localRequest, request: chain(noSignal, step), trusted: true, method: MethodLocal}
	}
	untrusted := func(name string, base func() Request, step func(*Request)) decideCase {
		return decideCase{name: name, base: base, request: chain(noSignal, step), reason: "non-browser signal"}
	}
	withQuery := func(r *Request) {} // query keys are not part of the decision input at all
	browserHeaders := chain(
		setHeader("Upgrade-Insecure-Requests", "1"),
		setHeader("Cache-Control", "no-cache"),
		setHeader("Pragma", "no-cache"),
		setHeader("Priority", "u=0, i"),
		setHeader("Purpose", "prefetch"),
		setHeader("Sec-Purpose", "prefetch"),
		setHeader("X-Requested-With", "XMLHttpRequest"),
		setHeader("Accept", "text/html"),
	)
	runDecideCases(t, []decideCase{
		trustedTailnet("keyless header", setHeader(KeylessHeader, "1")),
		trustedTailnet("bearer token", setHeader("Authorization", "Bearer x")),
		trustedTailnet("lower-case bearer scheme", setHeader("Authorization", "bearer some-client-key")),
		trustedTailnet("management key header", setHeader("X-Management-Key", "any")),
		trustedTailnet("anthropic key header", setHeader("X-Api-Key", "any")),
		trustedTailnet("google key header", setHeader("X-Goog-Api-Key", "any")),
		trustedLocal("local keyless header", setHeader(KeylessHeader, "1")),
		trustedLocal("local bearer token", setHeader("Authorization", "Bearer x")),
		untrusted("no origin, no sec-fetch, no signal", nil, func(*Request) {}),
		untrusted("query key only", nil, withQuery),
		untrusted("basic auth", nil, setHeader("Authorization", "Basic eDp5")),
		untrusted("negotiate auth", nil, setHeader("Authorization", "Negotiate abc")),
		untrusted("bare token", nil, setHeader("Authorization", "some-token")),
		untrusted("empty bearer", nil, setHeader("Authorization", "Bearer ")),
		untrusted("bearer with spaces only", nil, setHeader("Authorization", "Bearer    ")),
		untrusted("keyless header true", nil, setHeader(KeylessHeader, "true")),
		untrusted("keyless header 0", nil, setHeader(KeylessHeader, "0")),
		untrusted("keyless header empty", nil, setHeader(KeylessHeader, "")),
		untrusted("empty key headers", nil, chain(setHeader("X-Management-Key", " "), setHeader("X-Api-Key", ""), setHeader("X-Goog-Api-Key", ""))),
		untrusted("headers browsers add on their own", nil, browserHeaders),
		untrusted("top-level navigation", nil, setHeader("Sec-Fetch-Site", "none")),
		untrusted("local, no signal", localRequest, func(*Request) {}),
		untrusted("local navigation", localRequest, chain(setHeader("Sec-Fetch-Site", "none"), setHeader("Sec-Fetch-Mode", "navigate"))),
		untrusted("local same-origin subresource", localRequest, setHeader("Sec-Fetch-Site", "same-origin")),
	})
}

func TestDecide_R5b_QueryKeysNeverCount(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://"+testTailnetURL+"/v8/management/config?key=x&auth_token=y", nil)
	r.Header.Set("X-Forwarded-For", testDevice)
	r.Header.Set("Tailscale-User-Login", testLogin)
	got := Decide(testPolicy(), FromHTTP(testGateway, r))
	if got.Trusted || !strings.Contains(got.Reason, "non-browser signal") {
		t.Fatalf("decision = %+v, want untrusted for a query key", got)
	}
}

func TestDecide_R6_AM1_TailnetDevices(t *testing.T) {
	noIdentity := delHeader("Tailscale-User-Login", "Tailscale-User-Name")
	device := func(ip string) func(*Request) { return setHeader("X-Forwarded-For", ip) }
	runDecideCases(t, []decideCase{
		{name: "owner login, listed IP", trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "owner login, unlisted IP", request: device(testUnlisted), reason: "allowed-devices"},
		{name: "empty allowed-devices", policy: func(p *Policy) { p.AllowedDevices = nil }, reason: "allowed-devices"},
		{name: "blank allowed-devices entries", policy: func(p *Policy) { p.AllowedDevices = []string{"", " ", "not-an-ip"} }, reason: "allowed-devices"},
		{name: "no identity, listed IP (tagged node)", request: noIdentity, trusted: true, method: MethodTailnet, device: testDevice},
		{name: "no identity, unlisted IP", request: chain(noIdentity, device(testUnlisted)), reason: "allowed-devices"},
		{name: "listed IP, login not allowed", request: setHeader("Tailscale-User-Login", "someone-else@example.com"), reason: "allowed-logins"},
		{name: "empty allowed-logins, listed IP, no identity", request: noIdentity, policy: func(p *Policy) { p.AllowedLogins = nil }, reason: "allowed-logins is empty"},
		{name: "empty allowed-logins, owner login", policy: func(p *Policy) { p.AllowedLogins = []string{" "} }, reason: "allowed-logins is empty"},
		{name: "login matches case-insensitively", request: setHeader("Tailscale-User-Login", "Owner@Example.COM"), trusted: true, method: MethodTailnet, login: "Owner@Example.COM", device: testDevice},
		{name: "allowed-logins entry trimmed", policy: func(p *Policy) { p.AllowedLogins = []string{"  " + testLogin + "  "} }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "name without login", request: delHeader("Tailscale-User-Login"), reason: "without Tailscale-User-Login"},
		{name: "profile picture only", request: chain(noIdentity, setHeader("Tailscale-User-Profile-Pic", "https://example.com/p.png")), reason: "without Tailscale-User-Login"},
		{name: "empty login", request: setHeader("Tailscale-User-Login", ""), reason: "invalid Tailscale-User-Login"},
		{name: "unknown charset", request: setHeader("Tailscale-User-Login", "=?x-unknown?q?owner=40example.com?="), reason: "invalid Tailscale-User-Login"},
		{name: "IPv6 tailnet device", request: device(testDeviceV6), trusted: true, method: MethodTailnet, login: testLogin, device: testDeviceV6},
		{name: "IPv6 tailnet device, expanded form", request: device("fd7a:115c:a1e0:0:0:0:0:10"), trusted: true, method: MethodTailnet, login: testLogin, device: testDeviceV6},
		{name: "IPv6 device listed in expanded form", policy: func(p *Policy) { p.AllowedDevices = []string{"FD7A:115C:A1E0:0000::0010"} }, request: device(testDeviceV6), trusted: true, method: MethodTailnet, login: testLogin, device: testDeviceV6},
		{name: "IPv4-mapped tailnet address", request: device("::ffff:" + testDevice), trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
		{name: "two hops", request: device(testDevice + ", 100.64.0.11"), reason: "more than one address"},
		{name: "spoofed first hop", request: device("100.64.0.11," + testDevice), reason: "more than one address"},
		{name: "address with port", request: device(testDevice + ":1234"), reason: "invalid X-Forwarded-For"},
		{name: "bracketed IPv6", request: device("[" + testDeviceV6 + "]"), reason: "invalid X-Forwarded-For"},
		{name: "not an address", request: device("unknown"), reason: "invalid X-Forwarded-For"},
		{name: "empty forwarded-for", request: device(""), reason: "invalid X-Forwarded-For"},
		{name: "LAN address even when listed", policy: func(p *Policy) { p.AllowedDevices = []string{"192.168.1.20"} }, request: device("192.168.1.20"), reason: "not a tailnet address"},
		{name: "loopback address even when listed", policy: func(p *Policy) { p.AllowedDevices = []string{"127.0.0.1"} }, request: device("127.0.0.1"), reason: "not a tailnet address"},
		{name: "IPv6 outside the tailnet range", policy: func(p *Policy) { p.AllowedDevices = []string{"fd7a:115c:a1e1::10"} }, request: device("fd7a:115c:a1e1::10"), reason: "not a tailnet address"},
		{name: "tailnet Host without forwarded-for", request: delHeader("X-Forwarded-For"), reason: "without X-Forwarded-For"},
		{name: "tailnet Host without any proxy header", request: delHeader("X-Forwarded-For", "X-Forwarded-Host", "Tailscale-User-Login", "Tailscale-User-Name"), reason: "without X-Forwarded-For"},
		{name: "tailnet path ignores allow-local", policy: func(p *Policy) { p.AllowLocal = false }, trusted: true, method: MethodTailnet, login: testLogin, device: testDevice},
	})
}

func TestDecide_R6_EncodedLogin(t *testing.T) {
	const login = "jürgen@example.com"
	policy := testPolicy()
	policy.AllowedLogins = []string{login}
	for _, encoded := range []string{
		"=?utf-8?q?j=C3=BCrgen=40example=2Ecom?=",
		"=?UTF-8?Q?j=C3=BCrgen@example.com?=",
		"=?utf-8?b?asO8cmdlbkBleGFtcGxlLmNvbQ==?=",
		login,
	} {
		request := tailnetRequest()
		request.Header.Set("Tailscale-User-Login", encoded)
		got := Decide(policy, request)
		if !got.Trusted || got.Login != login {
			t.Fatalf("login %q: decision = %+v, want trusted as %q", encoded, got, login)
		}
	}
	request := tailnetRequest()
	request.Header.Set("Tailscale-User-Login", "=?utf-8?q?mallory=40example=2Ecom?=")
	if got := Decide(policy, request); got.Trusted {
		t.Fatalf("decoded login not in allowed-logins was trusted: %+v", got)
	}
}

func TestDecide_R7_AM4_LocalPath(t *testing.T) {
	trustedLocal := func(name string, host string, entries ...string) decideCase {
		return decideCase{
			name:    name,
			base:    localRequest,
			policy:  func(p *Policy) { p.AllowedHosts = append(p.AllowedHosts, entries...) },
			request: setHost(host),
			trusted: true,
			method:  MethodLocal,
		}
	}
	runDecideCases(t, []decideCase{
		trustedLocal("127.0.0.1 with port", "127.0.0.1:8317"),
		trustedLocal("localhost", "localhost:8317"),
		trustedLocal("localhost upper case with dot", "LOCALHOST.:8317"),
		trustedLocal("listed loopback alias", "127.0.0.2:8317", "127.0.0.2"),
		trustedLocal("listed .localhost name", "cpa.localhost:8317", "cpa.localhost"),
		trustedLocal("IPv4-mapped loopback", "[::ffff:127.0.0.1]:8317", "::ffff:127.0.0.1"),
		{name: "allow-local off", base: localRequest, policy: func(p *Policy) { p.AllowLocal = false }, reason: "allow-local is off"},
		{name: "loopback host not listed", base: localRequest, policy: func(p *Policy) { p.AllowedHosts = []string{testTailnetFQD, "localhost"} }, reason: "allowed-hosts"},
		{name: "empty allowed-logins does not affect local", base: localRequest, policy: func(p *Policy) { p.AllowedLogins = nil }, trusted: true, method: MethodLocal},
		{name: "empty allowed-devices does not affect local", base: localRequest, policy: func(p *Policy) { p.AllowedDevices = nil }, trusted: true, method: MethodLocal},
		{name: "forged forwarded-for", base: localRequest, request: setHeader("X-Forwarded-For", testDevice), reason: "X-Forwarded-For on a loopback Host"},
		{name: "localhost with X-Real-IP", base: localRequest, request: chain(setHost("localhost:8317"), setHeader("X-Real-IP", testDevice)), reason: "X-Real-Ip on a loopback Host"},
		{name: "Forwarded header", base: localRequest, request: setHeader("Forwarded", "for="+testDevice), reason: "Forwarded on a loopback Host"},
		{name: "forwarded host", base: localRequest, request: setHeader("X-Forwarded-Host", "127.0.0.1:8317"), reason: "X-Forwarded-Host on a loopback Host"},
		{name: "empty forwarded-for", base: localRequest, request: setHeader("X-Forwarded-For", ""), reason: "on a loopback Host"},
		{name: "forged login", base: localRequest, request: setHeader("Tailscale-User-Login", testLogin), reason: "Tailscale header"},
		{name: "forged name", base: localRequest, request: setHeader("Tailscale-User-Name", "Owner"), reason: "Tailscale header"},
		{name: "headers info", base: localRequest, request: setHeader("Tailscale-Headers-Info", "x"), reason: "Tailscale header"},
		{name: "non-canonical Tailscale header key", base: localRequest, request: func(r *Request) { r.Header["tailscale-user-login"] = []string{testLogin} }, reason: "Tailscale header"},
		{
			name: "cross-origin forged tailnet identity on 127.0.0.1",
			base: localRequest,
			request: chain(
				setHeader("Origin", "http://evil.example"),
				setHeader("X-Forwarded-For", testDevice),
				setHeader("Tailscale-User-Login", testLogin),
			),
			reason: "Origin does not match",
		},
		{
			name: "same-origin forged tailnet identity on 127.0.0.1",
			base: localRequest,
			request: chain(
				setHeader("X-Forwarded-For", testDevice),
				setHeader("Tailscale-User-Login", testLogin),
				setHeader("Tailscale-User-Name", "Owner"),
			),
			reason: "on a loopback Host",
		},
	})
}

func TestDecide_NilHeaderIsUntrusted(t *testing.T) {
	got := Decide(testPolicy(), Request{PeerIP: testGateway, Host: "127.0.0.1:8317"})
	if got.Trusted || !strings.Contains(got.Reason, "non-browser signal") {
		t.Fatalf("decision = %+v, want untrusted without headers", got)
	}
	if got := Decide(testPolicy(), FromHTTP(testGateway, nil)); got.Trusted {
		t.Fatalf("decision for a nil request = %+v, want untrusted", got)
	}
}

func TestDecision_Principal(t *testing.T) {
	cases := []struct {
		decision Decision
		want     string
	}{
		{Decision{Trusted: true, Method: MethodTailnet, Login: testLogin, Device: testDevice}, "tailnet:" + testLogin + "@" + testDevice},
		{Decision{Trusted: true, Method: MethodTailnet, Device: testDeviceV6}, "tailnet:@" + testDeviceV6},
		{Decision{Trusted: true, Method: MethodLocal}, "local"},
		{Decision{Reason: "x"}, ""},
	}
	for _, tc := range cases {
		if got := tc.decision.Principal(); got != tc.want {
			t.Errorf("Principal(%+v) = %q, want %q", tc.decision, got, tc.want)
		}
	}
	if got := Decide(testPolicy(), tailnetRequest()).Principal(); got != "tailnet:"+testLogin+"@"+testDevice {
		t.Fatalf("principal of the base tailnet request = %q", got)
	}
}

func TestPolicyFromConfig(t *testing.T) {
	if got := PolicyFromConfig(nil); got.Enabled {
		t.Fatalf("PolicyFromConfig(nil) = %+v, want disabled", got)
	}
	cfg := &config.Config{TrustedProxies: []string{"172.16.0.0/12"}}
	if got := PolicyFromConfig(cfg); got.Enabled || got.AllowLocal || len(got.AllowedHosts) != 0 {
		t.Fatalf("defaults = %+v, want everything off", got)
	}
	cfg.RemoteManagement.TailnetAuth = config.TailnetAuthConfig{
		Enabled:        true,
		AllowedLogins:  []string{testLogin},
		AllowedDevices: []string{testDevice},
		AllowedHosts:   []string{testTailnetFQD},
		AllowLocal:     true,
		ProxyAPI:       true,
	}
	got := PolicyFromConfig(cfg)
	if !got.Enabled || !got.AllowLocal || got.AllowedLogins[0] != testLogin || got.AllowedDevices[0] != testDevice ||
		got.AllowedHosts[0] != testTailnetFQD || got.TrustedProxies[0] != "172.16.0.0/12" {
		t.Fatalf("PolicyFromConfig = %+v", got)
	}
}

func TestFromHTTP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://"+testTailnetURL+"/v8/management/auth/session", nil)
	r.Header.Set(KeylessHeader, "1")
	got := FromHTTP(testGateway, r)
	if got.PeerIP != testGateway || got.Host != testTailnetURL || got.Header.Get(KeylessHeader) != "1" {
		t.Fatalf("FromHTTP = %+v", got)
	}
}
