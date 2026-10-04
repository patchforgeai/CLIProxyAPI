package api

// Full-stack tests for management.tailnet-auth: the real NewServer, a config file
// loaded by the real loader, and requests shaped exactly like tailscale serve
// forwards them (Docker gateway peer, X-Forwarded-For, X-Forwarded-Host and
// Tailscale-User-* headers) or like a direct request on this PC. Every value is
// fake: a CGNAT tailnet range, an example.com login and an example tailnet name.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

const (
	tailnetSrvKey       = "tailnet-server-management-key"
	tailnetSrvClientKey = "tailnet-server-client-key"
	tailnetSrvLogin     = "owner@example.com"
	tailnetSrvDevice    = "100.64.0.10"
	tailnetSrvUnlisted  = "100.64.0.99"
	tailnetSrvHost      = "devbox.example-tailnet.ts.net:8318"
	tailnetSrvPeer      = "172.17.0.1:41234" // Docker bridge gateway in front of the container
	tailnetSrvAuthFile  = "claude-owner.json"
)

// tailnetSrvBlock is management.tailnet-auth as tailnet-trust.ps1 writes it.
const tailnetSrvBlock = `  tailnet-auth:
    enabled: true
    allowed-logins:
      - "owner@example.com"
    allowed-devices:
      - "100.64.0.10"
      - "fd7a:115c:a1e0::10"
    allowed-hosts:
      - "devbox"
      - "devbox.example-tailnet.ts.net"
      - "localhost"
      - "127.0.0.1"
    allow-local: true
    proxy-api: true
`

type tailnetSrvFixture struct {
	server     *Server
	configPath string
	authDir    string
}

// newTailnetSrvFixture writes a v8 config with the given tailnet-auth block (two
// spaces deep under management), loads it with the real loader and starts the
// real server stack. The management key is pre-hashed at the minimum bcrypt cost
// to keep the many key checks fast.
func newTailnetSrvFixture(t *testing.T, block string, mutate func(*config.Config), opts ...ServerOption) *tailnetSrvFixture {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth dir: %v", errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(authDir, tailnetSrvAuthFile), []byte(`{"type":"claude","email":"owner@example.com"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	hash, errHash := bcrypt.GenerateFromPassword([]byte(tailnetSrvKey), bcrypt.MinCost)
	if errHash != nil {
		t.Fatalf("hash management key: %v", errHash)
	}
	configPath := filepath.Join(dir, "config.yaml")
	rawConfig := fmt.Sprintf(`config-version: 8
server:
  port: 8317
  trusted-proxies:
    - 127.0.0.1
    - 172.16.0.0/12
management:
  allow-remote: true
  secret-key: %q
%saccess:
  api-keys:
    - %q
oauth:
  auth-dir: %q
`, string(hash), block, tailnetSrvClientKey, filepath.ToSlash(authDir))
	if errWrite := os.WriteFile(configPath, []byte(rawConfig), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	cfg, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	cfg.Debug = true
	if mutate != nil {
		mutate(cfg)
	}
	server := NewServer(cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), configPath, opts...)
	return &tailnetSrvFixture{server: server, configPath: configPath, authDir: authDir}
}

// tailnetSrvRequest builds a request of one of these shapes:
//
//	tailnet  - an allowed device of the owner through tailscale serve, with X-CPA-Keyless
//	local    - a direct request on this PC to 127.0.0.1:8317, with X-CPA-Keyless
//	origin   - the tailnet shape sent cross-origin (Origin: http://evil.example)
//	device   - the tailnet shape from a device that is not in allowed-devices
//	signal   - the tailnet shape without Origin and without any non-browser signal
//	plain    - no proxy headers, no signal, Host 127.0.0.1:8317 (curl without a key)
func tailnetSrvRequest(t *testing.T, shape, method, path, body string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = tailnetSrvPeer
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	tailnet := func() {
		req.Host = tailnetSrvHost
		req.Header.Set("X-Forwarded-For", tailnetSrvDevice)
		req.Header.Set("X-Forwarded-Host", tailnetSrvHost)
		req.Header.Set("Tailscale-User-Login", tailnetSrvLogin)
		req.Header.Set("Tailscale-User-Name", "Owner")
		req.Header.Set("X-CPA-Keyless", "1")
	}
	switch shape {
	case "tailnet":
		tailnet()
	case "local":
		req.Host = "127.0.0.1:8317"
		req.Header.Set("X-CPA-Keyless", "1")
	case "origin":
		tailnet()
		req.Header.Set("Origin", "http://evil.example")
	case "device":
		tailnet()
		req.Header.Set("X-Forwarded-For", tailnetSrvUnlisted)
	case "signal":
		tailnet()
		req.Header.Del("X-CPA-Keyless")
	case "plain":
		req.Host = "127.0.0.1:8317"
	default:
		t.Fatalf("unknown request shape %q", shape)
	}
	return req
}

func (f *tailnetSrvFixture) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.server.engine.ServeHTTP(rec, req)
	return rec
}

func newTailnetSrvUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// AM9: token-bearing routes are never key-optional for an untrusted request, on
// /v0 and on /v8, and they work without a key for a trusted one.
func TestTailnetAuthServer_TokenBearingRoutes(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	upstream := newTailnetSrvUpstream(t)
	apiCallBody := fmt.Sprintf(`{"method":"GET","url":%q}`, upstream.URL+"/ping")

	routes := []struct {
		method, path, body string
		check              func(t *testing.T, rec *httptest.ResponseRecorder)
	}{
		{http.MethodGet, "/v8/management/credentials/download?name=" + tailnetSrvAuthFile, "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			if !strings.Contains(rec.Body.String(), `"type":"claude"`) {
				t.Fatalf("download body = %s", rec.Body.String())
			}
		}},
		{http.MethodGet, "/v0/management/auth-files/download?name=" + tailnetSrvAuthFile, "", nil},
		{http.MethodPost, "/v8/management/requests/api-call", apiCallBody, func(t *testing.T, rec *httptest.ResponseRecorder) {
			var payload struct {
				StatusCode int    `json:"status_code"`
				Body       string `json:"body"`
			}
			if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil || payload.StatusCode != http.StatusOK || payload.Body != "pong" {
				t.Fatalf("api-call payload = %+v (%v); body = %s", payload, errDecode, rec.Body.String())
			}
		}},
		{http.MethodPost, "/v0/management/api-call", apiCallBody, nil},
		{http.MethodGet, "/v8/management/config", "", func(t *testing.T, rec *httptest.ResponseRecorder) {
			if !strings.Contains(rec.Body.String(), `"tailnet-auth"`) {
				t.Fatalf("config body lacks tailnet-auth: %s", rec.Body.String())
			}
		}},
		{http.MethodGet, "/v0/management/config", "", nil},
		{http.MethodGet, "/v0/management/auth-files", "", nil},
	}
	for _, route := range routes {
		for _, shape := range []string{"origin", "device", "signal", "plain"} {
			rec := f.serve(tailnetSrvRequest(t, shape, route.method, route.path, route.body))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s (%s, no key): status = %d, want 401; body = %s", route.method, route.path, shape, rec.Code, rec.Body.String())
			}
		}
		for _, shape := range []string{"tailnet", "local"} {
			rec := f.serve(tailnetSrvRequest(t, shape, route.method, route.path, route.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s (%s, no key): status = %d, want 200; body = %s", route.method, route.path, shape, rec.Code, rec.Body.String())
			}
			if route.check != nil {
				route.check(t, rec)
			}
		}
		keyed := tailnetSrvRequest(t, "device", route.method, route.path, route.body)
		keyed.Header.Set("Authorization", "Bearer "+tailnetSrvKey)
		if rec := f.serve(keyed); rec.Code != http.StatusOK {
			t.Fatalf("%s %s (untrusted, key): status = %d, want 200; body = %s", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
}

// T3 Code's hub keeps sending its stored key, right or wrong; from a trusted
// source the request is trusted and the key is ignored.
func TestTailnetAuthServer_T3HubAnyKeyFromTrustedSources(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	for _, shape := range []string{"tailnet", "local"} {
		for _, key := range []string{tailnetSrvKey, "stale-hub-key"} {
			req := tailnetSrvRequest(t, shape, http.MethodGet, "/v0/management/auth-files", "")
			req.Header.Del("X-CPA-Keyless")
			req.Header.Set("Authorization", "Bearer "+key)
			rec := f.serve(req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"files"`) {
				t.Fatalf("%s with key %q: status = %d body = %s, want 200 with files", shape, key, rec.Code, rec.Body.String())
			}
		}
	}
	req := tailnetSrvRequest(t, "device", http.MethodGet, "/v0/management/auth-files", "")
	req.Header.Set("Authorization", "Bearer stale-hub-key")
	if rec := f.serve(req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key from an unlisted device: status = %d, want 401", rec.Code)
	}
}

func TestTailnetAuthServer_SessionEndpoint(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	const path = "/v8/management/auth/session"
	session := func(req *http.Request) (int, map[string]any) {
		rec := f.serve(req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	status, body := session(tailnetSrvRequest(t, "tailnet", http.MethodGet, path, ""))
	if status != http.StatusOK || body["authenticated"] != true || body["method"] != "tailnet" || body["login"] != tailnetSrvLogin || body["device"] != tailnetSrvDevice {
		t.Fatalf("tailnet session: %d %v", status, body)
	}
	status, body = session(tailnetSrvRequest(t, "local", http.MethodGet, path, ""))
	if status != http.StatusOK || body["method"] != "local" || body["login"] != "" || body["device"] != "" {
		t.Fatalf("local session: %d %v", status, body)
	}
	keyed := tailnetSrvRequest(t, "device", http.MethodGet, path, "")
	keyed.Header.Set("X-Management-Key", tailnetSrvKey)
	status, body = session(keyed)
	if status != http.StatusOK || body["method"] != "key" || body["login"] != "" || body["device"] != "" {
		t.Fatalf("key session: %d %v", status, body)
	}

	// Ten keyless probes from an untrusted client never ban it.
	for i := 0; i < 10; i++ {
		if status, _ = session(tailnetSrvRequest(t, "device", http.MethodGet, path, "")); status != http.StatusUnauthorized {
			t.Fatalf("untrusted probe %d: status = %d, want 401", i+1, status)
		}
	}
	keyed = tailnetSrvRequest(t, "device", http.MethodGet, path, "")
	keyed.Header.Set("Authorization", "Bearer "+tailnetSrvKey)
	if status, _ = session(keyed); status != http.StatusOK {
		t.Fatalf("key after ten keyless probes: status = %d, want 200 (banned?)", status)
	}

	// The probe is a v8-only route.
	if rec := f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v0/management/auth/session", "")); rec.Code != http.StatusNotFound {
		t.Fatalf("/v0 session probe: status = %d, want 404", rec.Code)
	}
}

// AM4: trusted management responses drop CORS and are never cached; key-
// authenticated management responses and the proxy API keep today's headers.
func TestTailnetAuthServer_CORSHeaders(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)

	trusted := f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v8/management/auth/session", ""))
	if trusted.Code != http.StatusOK {
		t.Fatalf("trusted session: status = %d", trusted.Code)
	}
	for name := range trusted.Header() {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Fatalf("trusted management response carries %s", name)
		}
	}
	if got := trusted.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("trusted Cache-Control = %q, want no-store", got)
	}
	if got := trusted.Header().Get("X-CPA-VERSION"); got == "" && trusted.Header().Get("X-Cpa-Version") == "" {
		t.Log("X-CPA-VERSION is empty in tests; the header itself is still set by the middleware")
	}

	keyed := tailnetSrvRequest(t, "device", http.MethodGet, "/v8/management/auth/session", "")
	keyed.Header.Set("Authorization", "Bearer "+tailnetSrvKey)
	if rec := f.serve(keyed); rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("key-authenticated management: status = %d ACAO = %q, want 200 and *", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}

	proxy := f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/models", ""))
	if proxy.Code != http.StatusOK || proxy.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("keyless proxy API: status = %d ACAO = %q, want 200 and *", proxy.Code, proxy.Header().Get("Access-Control-Allow-Origin"))
	}
}

// OAuth callbacks stay unauthenticated and state-matched; tailnet trust neither
// guards nor opens them.
func TestTailnetAuthServer_OAuthCallbacksAnswerWithoutAKey(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)

	for i, route := range []struct{ method, path string }{
		{http.MethodGet, "/v0/management/oauth-callback"},
		{http.MethodPost, "/v0/management/oauth-callback"},
		{http.MethodGet, "/v8/management/oauth/callback"},
		{http.MethodPost, "/v8/management/oauth/callback"},
	} {
		state := fmt.Sprintf("tailnet-oauth-state-%d", i)
		if errRegister := managementHandlers.RegisterPluginOAuthSession(state, "gemini-cli", nil); errRegister != nil {
			t.Fatalf("register oauth session: %v", errRegister)
		}
		t.Cleanup(func() { managementHandlers.CompleteOAuthSession(state) })
		var req *http.Request
		if route.method == http.MethodGet {
			req = tailnetSrvRequest(t, "plain", route.method, route.path+"?state="+state+"&code=test-code", "")
		} else {
			req = tailnetSrvRequest(t, "plain", route.method, route.path, `{"state":"`+state+`","code":"test-code"}`)
		}
		rec := f.serve(req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s without a key: status = %d, want 200; body = %s", route.method, route.path, rec.Code, rec.Body.String())
		}
		if _, errRead := os.ReadFile(filepath.Join(f.authDir, ".oauth-gemini-cli-"+state+".oauth")); errRead != nil {
			t.Fatalf("%s %s did not write the callback file: %v", route.method, route.path, errRead)
		}
	}

	for _, path := range []string{"/anthropic/callback", "/codex/callback", "/antigravity/callback", "/callback", "/devin/callback"} {
		for _, shape := range []string{"plain", "origin"} {
			rec := f.serve(tailnetSrvRequest(t, shape, http.MethodGet, path+"?state=unknown-state", ""))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || strings.Contains(rec.Body.String(), "management key") {
				t.Fatalf("%s (%s): status = %d body = %s, want the callback's own answer", path, shape, rec.Code, rec.Body.String())
			}
		}
	}
}

// AM6: the proxy API accepts trusted requests without an API key and attributes
// them to a stable per-device principal; a valid key always wins.
func TestTailnetAuthServer_ProxyAPIPrincipal(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	f.server.engine.GET("/devyre-test/principal", AuthMiddleware(f.server.accessManager), func(c *gin.Context) {
		metadata, _ := c.Get("accessMetadata")
		c.JSON(http.StatusOK, gin.H{
			"principal": c.GetString("userApiKey"),
			"provider":  c.GetString("accessProvider"),
			"metadata":  metadata,
		})
	})
	type principal struct {
		Principal string            `json:"principal"`
		Provider  string            `json:"provider"`
		Metadata  map[string]string `json:"metadata"`
	}
	call := func(req *http.Request) (int, principal) {
		rec := f.serve(req)
		var got principal
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got
	}
	request := func(shape, key string) *http.Request {
		req := tailnetSrvRequest(t, shape, http.MethodGet, "/devyre-test/principal", "")
		if key != "" {
			req.Header.Del("X-CPA-Keyless")
			req.Header.Set("Authorization", "Bearer "+key)
		}
		return req
	}

	status, got := call(request("tailnet", tailnetSrvClientKey))
	if status != http.StatusOK || got.Principal != tailnetSrvClientKey || got.Provider != sdkaccess.DefaultAccessProviderName {
		t.Fatalf("trusted with a valid key: %d %+v, want the key as principal", status, got)
	}
	wantTailnet := "tailnet:" + tailnetSrvLogin + "@" + tailnetSrvDevice
	for _, key := range []string{"", "not-a-configured-key"} {
		status, got = call(request("tailnet", key))
		if status != http.StatusOK || got.Principal != wantTailnet || got.Provider != "tailnet-auth" || got.Metadata["source"] != "tailnet" {
			t.Fatalf("trusted with key %q: %d %+v, want %s from tailnet-auth", key, status, got, wantTailnet)
		}
	}
	tagged := request("tailnet", "")
	tagged.Header.Del("Tailscale-User-Login")
	tagged.Header.Del("Tailscale-User-Name")
	if status, got = call(tagged); status != http.StatusOK || got.Principal != "tailnet:@"+tailnetSrvDevice {
		t.Fatalf("listed tagged device: %d %+v", status, got)
	}
	ipv6 := request("tailnet", "")
	ipv6.Header.Set("X-Forwarded-For", "fd7a:115c:a1e0:0:0:0:0:10")
	if status, got = call(ipv6); status != http.StatusOK || got.Principal != "tailnet:"+tailnetSrvLogin+"@fd7a:115c:a1e0::10" {
		t.Fatalf("IPv6 device: %d %+v", status, got)
	}
	if status, got = call(request("local", "")); status != http.StatusOK || got.Principal != "local" || got.Provider != "tailnet-auth" || got.Metadata["source"] != "local" {
		t.Fatalf("local: %d %+v", status, got)
	}
	for _, shape := range []string{"origin", "device", "signal", "plain"} {
		if status, _ = call(request(shape, "")); status != http.StatusUnauthorized {
			t.Fatalf("untrusted %s without a key: status = %d, want 401", shape, status)
		}
	}
	if status, _ = call(request("device", "not-a-configured-key")); status != http.StatusUnauthorized {
		t.Fatalf("untrusted with an invalid key: status = %d, want 401", status)
	}

	// Real routes behave the same.
	if rec := f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/models", "")); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models trusted without a key: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.serve(tailnetSrvRequest(t, "device", http.MethodGet, "/v1/models", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/models untrusted without a key: status = %d, want 401", rec.Code)
	}
	if rec := f.serve(tailnetSrvRequest(t, "local", http.MethodGet, "/v1beta/models", "")); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1beta/models local without a key: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

type tailnetSrvProvider struct{ err *sdkaccess.AuthError }

func (p tailnetSrvProvider) Identifier() string { return "tailnet-test-provider" }

func (p tailnetSrvProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, p.err
}

// Only "no credentials" and "invalid credential" (401) fall back to tailnet
// trust; any other access-provider failure aborts exactly as before.
func TestTailnetAuthServer_ProxyAPIOnlyReplacesMissingOrInvalidKeys(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	cases := []struct {
		name   string
		err    *sdkaccess.AuthError
		status int
	}{
		{"internal error", sdkaccess.NewInternalAuthError("provider down", nil), http.StatusInternalServerError},
		{"forbidden", &sdkaccess.AuthError{Code: "forbidden", Message: "blocked", StatusCode: http.StatusForbidden}, http.StatusForbidden},
		{"401 with another code", &sdkaccess.AuthError{Code: "expired", Message: "expired", StatusCode: http.StatusUnauthorized}, http.StatusUnauthorized},
		{"not handled", sdkaccess.NewNotHandledError(), http.StatusOK},
		{"invalid credential", sdkaccess.NewInvalidCredentialError(), http.StatusOK},
	}
	for i, tc := range cases {
		manager := sdkaccess.NewManager()
		manager.SetProviders([]sdkaccess.Provider{tailnetSrvProvider{err: tc.err}})
		path := fmt.Sprintf("/devyre-test/provider-%d", i)
		f.server.engine.GET(path, AuthMiddleware(manager), func(c *gin.Context) {
			c.String(http.StatusOK, c.GetString("userApiKey"))
		})
		rec := f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, path, ""))
		if rec.Code != tc.status {
			t.Fatalf("%s: status = %d, want %d; body = %s", tc.name, rec.Code, tc.status, rec.Body.String())
		}
		if tc.status == http.StatusOK && rec.Body.String() != "tailnet:"+tailnetSrvLogin+"@"+tailnetSrvDevice {
			t.Fatalf("%s: principal = %q", tc.name, rec.Body.String())
		}
	}
}

// devyreKeylessProxyAccess also refuses a "no credentials" error that does not
// carry 401, should a future access manager ever produce one.
func TestDevyreKeylessProxyAccess_RequiresA401(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/models", "")
	c.Set(devyreServerContextKey, f.server)
	odd := &sdkaccess.AuthError{Code: sdkaccess.AuthErrorCodeNoCredentials, Message: "missing", StatusCode: http.StatusForbidden}
	if devyreKeylessProxyAccess(c, odd) {
		t.Fatal("a non-401 error was replaced by tailnet trust")
	}
	if devyreKeylessProxyAccess(c, nil) {
		t.Fatal("a nil error was treated as a failed key check")
	}
	if !devyreKeylessProxyAccess(c, sdkaccess.NewNoCredentialsError()) || c.GetString("userApiKey") != "tailnet:"+tailnetSrvLogin+"@"+tailnetSrvDevice {
		t.Fatalf("a trusted request without a key was not accepted: principal %q", c.GetString("userApiKey"))
	}
	keyOnly, _ := gin.CreateTestContext(httptest.NewRecorder())
	keyOnly.Request = tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/ws", "")
	keyOnly.Set(devyreServerContextKey, f.server)
	keyOnly.Set(devyreKeyOnlyContextKey, true)
	if devyreKeylessProxyAccess(keyOnly, sdkaccess.NewNoCredentialsError()) {
		t.Fatal("a key-only route accepted tailnet trust")
	}
	bare, _ := gin.CreateTestContext(httptest.NewRecorder())
	bare.Request = tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/models", "")
	if devyreKeylessProxyAccess(bare, sdkaccess.NewNoCredentialsError()) {
		t.Fatal("a request outside a server (no live config) accepted tailnet trust")
	}
}

func TestTailnetAuthServer_ProxyAPIOffKeepsKeysRequired(t *testing.T) {
	block := strings.Replace(tailnetSrvBlock, "proxy-api: true", "proxy-api: false", 1)
	f := newTailnetSrvFixture(t, block, nil)
	for _, shape := range []string{"tailnet", "local"} {
		if rec := f.serve(tailnetSrvRequest(t, shape, http.MethodGet, "/v1/models", "")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s /v1/models with proxy-api off: status = %d, want 401", shape, rec.Code)
		}
		if rec := f.serve(tailnetSrvRequest(t, shape, http.MethodGet, "/v8/management/auth/session", "")); rec.Code != http.StatusOK {
			t.Fatalf("%s management with proxy-api off: status = %d, want 200", shape, rec.Code)
		}
	}
	keyed := tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/models", "")
	keyed.Header.Set("Authorization", "Bearer "+tailnetSrvClientKey)
	if rec := f.serve(keyed); rec.Code != http.StatusOK {
		t.Fatalf("/v1/models with a valid key: status = %d, want 200", rec.Code)
	}

	// The proxy API reads the live server config: a reload turns keyless access on.
	next := *f.server.getConfig()
	next.RemoteManagement.TailnetAuth.ProxyAPI = true
	f.server.UpdateClients(&next)
	if rec := f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v1/models", "")); rec.Code != http.StatusOK {
		t.Fatalf("/v1/models after enabling proxy-api: status = %d, want 200", rec.Code)
	}
}

func TestTailnetAuthServer_DisabledByDefault(t *testing.T) {
	f := newTailnetSrvFixture(t, "", nil)
	if f.server.getConfig().RemoteManagement.TailnetAuth.Enabled {
		t.Fatal("tailnet-auth enabled without a config block")
	}
	for _, shape := range []string{"tailnet", "local"} {
		for _, path := range []string{"/v8/management/auth/session", "/v0/management/auth-files", "/v1/models"} {
			if rec := f.serve(tailnetSrvRequest(t, shape, http.MethodGet, path, "")); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s without the block: status = %d, want 401", shape, path, rec.Code)
			}
		}
	}
}

// AM7: every keyless proxy upgrade passes the browser guard, and /v1/ws is never keyless.
func TestTailnetAuthServer_WebsocketUpgrades(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, func(cfg *config.Config) { cfg.WebsocketAuth = true })
	f.server.AttachWebsocketRoute("/v1/ws", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("relay"))
	}))
	upgrade := func(shape, path, origin string) *http.Request {
		req := tailnetSrvRequest(t, shape, http.MethodGet, path, "")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return req
	}

	for _, path := range []string{"/v1/responses", "/v1/realtime?model=gpt-realtime", "/backend-api/codex/responses"} {
		for _, origin := range []string{"http://evil.example", "null"} {
			for _, shape := range []string{"tailnet", "local"} {
				if rec := f.serve(upgrade(shape, path, origin)); rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s upgrade from %s with Origin %s and no key: status = %d, want 401", path, shape, origin, rec.Code)
				}
			}
		}
	}
	// A trusted, same-origin handshake passes authentication (and then fails the
	// upgrade on the test recorder, which cannot hijack).
	if rec := f.serve(upgrade("tailnet", "/v1/realtime?model=gpt-realtime", "http://"+tailnetSrvHost)); rec.Code == http.StatusUnauthorized {
		t.Fatalf("trusted same-origin realtime handshake: status = 401, want it past authentication")
	}

	for _, shape := range []string{"tailnet", "local"} {
		if rec := f.serve(upgrade(shape, "/v1/ws", "")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("trusted /v1/ws upgrade from %s without a key, ws-auth on: status = %d, want 401", shape, rec.Code)
		}
	}
	keyed := upgrade("tailnet", "/v1/ws", "")
	keyed.Header.Set("Authorization", "Bearer "+tailnetSrvClientKey)
	if rec := f.serve(keyed); rec.Code != http.StatusOK || rec.Body.String() != "relay" {
		t.Fatalf("/v1/ws with a valid key: status = %d body = %s, want the relay", rec.Code, rec.Body.String())
	}
}

// AM5: the pages that load the keyless panel cannot be framed.
func TestTailnetAuthServer_PanelPagesDenyFraming(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if errWrite := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte("<html>management app</html>"), 0o600); errWrite != nil {
		t.Fatalf("write management asset: %v", errWrite)
	}
	assertNoFraming := func(t *testing.T, name string, rec *httptest.ResponseRecorder, wantBody string) {
		t.Helper()
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wantBody) {
			t.Fatalf("%s: status = %d body = %s, want 200 containing %q", name, rec.Code, rec.Body.String(), wantBody)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Fatalf("%s: Content-Security-Policy = %q", name, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s: X-Frame-Options = %q", name, got)
		}
	}

	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	assertNoFraming(t, "panel", f.serve(tailnetSrvRequest(t, "tailnet", http.MethodGet, "/management.html", "")), "management app")

	safe := newTailnetSrvFixture(t, tailnetSrvBlock, nil, WithExampleAPIKeySafeMode())
	cfg := *safe.server.getConfig()
	cfg.APIKeys = []string{"your-api-key-1"}
	safe.server.UpdateClients(&cfg)
	assertNoFraming(t, "safe-mode root", safe.serve(tailnetSrvRequest(t, "local", http.MethodGet, "/", "")), "Example API key detected")
	assertNoFraming(t, "safe-mode panel", safe.serve(tailnetSrvRequest(t, "local", http.MethodGet, "/management.html", "")), "Example API key detected")
	assertNoFraming(t, "safe-mode configure", safe.serve(tailnetSrvRequest(t, "local", http.MethodGet, "/management.html?safe-mode=configure", "")), "management app")
	head := safe.serve(tailnetSrvRequest(t, "local", http.MethodHead, "/management.html", ""))
	if head.Header().Get("X-Frame-Options") != "DENY" || head.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
		t.Fatalf("safe-mode HEAD lacks anti-framing headers: %v", head.Header())
	}
}

// A4: a write through the /v8 config path changes management trust at once.
func TestTailnetAuthServer_V8ConfigWriteTakesEffect(t *testing.T) {
	f := newTailnetSrvFixture(t, tailnetSrvBlock, nil)
	newDevice := func() *http.Request {
		req := tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v8/management/auth/session", "")
		req.Header.Set("X-Forwarded-For", "100.64.0.20")
		return req
	}
	if rec := f.serve(newDevice()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unlisted device before the write: status = %d, want 401", rec.Code)
	}

	block := `{"enabled":true,"allowed-logins":["owner@example.com"],"allowed-devices":["100.64.0.10","100.64.0.20"],` +
		`"allowed-hosts":["devbox","devbox.example-tailnet.ts.net","localhost","127.0.0.1"],"allow-local":true,"proxy-api":true}`
	write := tailnetSrvRequest(t, "device", http.MethodPut, "/v8/management/config/management/tailnet-auth", block)
	write.Header.Set("Authorization", "Bearer "+tailnetSrvKey)
	if rec := f.serve(write); rec.Code != http.StatusOK {
		t.Fatalf("PUT tailnet-auth: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.serve(newDevice()); rec.Code != http.StatusOK {
		t.Fatalf("newly listed device after the write: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	read := tailnetSrvRequest(t, "tailnet", http.MethodGet, "/v8/management/config/management/tailnet-auth", "")
	rec := f.serve(read)
	var view map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &view); rec.Code != http.StatusOK || errDecode != nil {
		t.Fatalf("GET tailnet-auth: status = %d (%v); body = %s", rec.Code, errDecode, rec.Body.String())
	}
	if devices, _ := view["allowed-devices"].([]any); len(devices) != 2 || view["enabled"] != true {
		t.Fatalf("GET tailnet-auth = %v", view)
	}
	reloaded, errLoad := config.LoadConfig(f.configPath)
	if errLoad != nil {
		t.Fatalf("reload written config: %v", errLoad)
	}
	if got := reloaded.RemoteManagement.TailnetAuth; len(got.AllowedDevices) != 2 || !got.Enabled || !got.AllowLocal || !got.ProxyAPI {
		t.Fatalf("written tailnet-auth = %+v", got)
	}
}
