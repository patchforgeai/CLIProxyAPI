package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tailnetauth"
	"golang.org/x/crypto/bcrypt"
)

// Fake values only: a CGNAT tailnet range, an example.com login and an example
// tailnet name.
const (
	tailnetTestKey      = "tailnet-test-management-key"
	tailnetTestLogin    = "owner@example.com"
	tailnetTestDevice   = "100.64.0.10"
	tailnetTestUnlisted = "100.64.0.99"
	tailnetTestHost     = "devbox.example-tailnet.ts.net:8318"
	tailnetTestGateway  = "172.19.0.1:41234" // Docker bridge gateway in front of the container
)

func tailnetTestHash(t *testing.T) string {
	t.Helper()
	hash, errHash := bcrypt.GenerateFromPassword([]byte(tailnetTestKey), bcrypt.MinCost)
	if errHash != nil {
		t.Fatalf("hash management key: %v", errHash)
	}
	return string(hash)
}

func newTailnetTestHandler(t *testing.T, mutate func(*config.Config)) *Handler {
	t.Helper()
	cfg := &config.Config{Port: 8317, TrustedProxies: []string{"127.0.0.1", "172.16.0.0/12"}}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.SecretKey = tailnetTestHash(t)
	cfg.RemoteManagement.TailnetAuth = config.TailnetAuthConfig{
		Enabled:        true,
		AllowedLogins:  []string{tailnetTestLogin},
		AllowedDevices: []string{tailnetTestDevice},
		AllowedHosts:   []string{"devbox:8318", tailnetTestHost, "localhost", "127.0.0.1"},
		AllowLocal:     true,
		ProxyAPI:       true,
	}
	if mutate != nil {
		mutate(cfg)
	}
	return &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo)}
}

// newTailnetTestRouter mirrors the production stack: corsMiddleware sets CORS
// headers on every response, then the management middleware runs.
func newTailnetTestRouter(t *testing.T, h *Handler) *gin.Engine {
	t.Helper()
	router := gin.New()
	if errProxies := router.SetTrustedProxies([]string{"127.0.0.1", "172.16.0.0/12"}); errProxies != nil {
		t.Fatalf("set trusted proxies: %v", errProxies)
	}
	router.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Expose-Headers", "X-CPA-VERSION")
		c.Next()
	})
	ok := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }
	v0 := router.Group("/v0/management", h.Middleware())
	v0.GET("/auth-files", ok)
	v0.POST("/api-call", ok)
	v8 := router.Group("/v8/management", h.Middleware())
	v8.GET("/config", ok)
	v8.GET("/auth/session", h.GetAuthSession)
	return router
}

type tailnetTestRequest struct {
	method string
	path   string
	peer   string
	host   string
	header map[string]string
}

// tailnetTrusted is a request from an allowed device of the owner, exactly as
// tailscale serve forwards it, sent with the X-CPA-Keyless signal.
func tailnetTrusted(path string) tailnetTestRequest {
	return tailnetTestRequest{
		method: http.MethodGet,
		path:   path,
		peer:   tailnetTestGateway,
		host:   tailnetTestHost,
		header: map[string]string{
			"X-Forwarded-For":      tailnetTestDevice,
			"X-Forwarded-Host":     tailnetTestHost,
			"Tailscale-User-Login": tailnetTestLogin,
			"Tailscale-User-Name":  "Owner",
			"X-CPA-Keyless":        "1",
		},
	}
}

// tailnetLocal is a direct request from this PC to the published loopback port.
func tailnetLocal(path string) tailnetTestRequest {
	return tailnetTestRequest{
		method: http.MethodGet,
		path:   path,
		peer:   tailnetTestGateway,
		host:   "127.0.0.1:8317",
		header: map[string]string{"X-CPA-Keyless": "1"},
	}
}

func (r tailnetTestRequest) with(name, value string) tailnetTestRequest {
	header := make(map[string]string, len(r.header)+1)
	for key, existing := range r.header {
		header[key] = existing
	}
	if value == "" {
		delete(header, name)
	} else {
		header[name] = value
	}
	r.header = header
	return r
}

func (r tailnetTestRequest) withPeer(peer string) tailnetTestRequest {
	r.peer = peer
	return r
}

func (r tailnetTestRequest) serve(router *gin.Engine) *httptest.ResponseRecorder {
	req := httptest.NewRequest(r.method, r.path, nil)
	req.RemoteAddr = r.peer
	req.Host = r.host
	for name, value := range r.header {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func (h *Handler) tailnetTestFailures() int {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	total := 0
	for _, info := range h.failedAttempts {
		total += info.count
		if !info.blockedUntil.IsZero() {
			total += 1000
		}
	}
	return total
}

func TestTailnetAuthMiddleware_TrustedRequestsSkipTheKey(t *testing.T) {
	h := newTailnetTestHandler(t, nil)
	router := newTailnetTestRouter(t, h)

	for _, request := range []tailnetTestRequest{
		tailnetTrusted("/v0/management/auth-files"),
		tailnetTrusted("/v8/management/config"),
		tailnetLocal("/v0/management/auth-files"),
		tailnetLocal("/v8/management/config"),
		// T3 Code's hub keeps sending whatever key it has; trust wins.
		tailnetTrusted("/v0/management/auth-files").with("X-CPA-Keyless", "").with("Authorization", "Bearer stale-or-wrong-key"),
		tailnetLocal("/v8/management/config").with("X-CPA-Keyless", "").with("Authorization", "Bearer "+tailnetTestKey),
	} {
		rec := request.serve(router)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s from %s: status = %d, want 200; body = %s", request.path, request.host, rec.Code, rec.Body.String())
		}
		for name := range rec.Header() {
			if strings.HasPrefix(name, "Access-Control-") {
				t.Fatalf("%s: trusted response still carries %s", request.path, name)
			}
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", request.path, got)
		}
	}
	if failures := h.tailnetTestFailures(); failures != 0 {
		t.Fatalf("trusted requests touched the failure counter: %d", failures)
	}
}

func TestTailnetAuthMiddleware_UntrustedRequestsStillNeedTheKey(t *testing.T) {
	h := newTailnetTestHandler(t, nil)
	router := newTailnetTestRouter(t, h)

	untrusted := map[string]struct {
		request tailnetTestRequest
		// A key header is itself a non-browser signal, so a request that only
		// lacked the signal becomes trusted once it carries the key.
		trustedWithKey bool
	}{
		"unlisted device":   {request: tailnetTrusted("/v8/management/config").with("X-Forwarded-For", tailnetTestUnlisted)},
		"cross-origin":      {request: tailnetTrusted("/v8/management/config").with("Origin", "http://evil.example")},
		"no R5b signal":     {request: tailnetTrusted("/v8/management/config").with("X-CPA-Keyless", ""), trustedWithKey: true},
		"funnel":            {request: tailnetTrusted("/v8/management/config").with("Tailscale-Funnel-Request", "?1")},
		"forged on 127.0.0": {request: tailnetLocal("/v8/management/config").with("X-Forwarded-For", tailnetTestDevice).with("Tailscale-User-Login", tailnetTestLogin)},
		"LAN peer":          {request: tailnetLocal("/v8/management/config").withPeer("192.168.1.20:5555")},
		"v0 unlisted":       {request: tailnetTrusted("/v0/management/auth-files").with("X-Forwarded-For", tailnetTestUnlisted)},
	}
	for name, tc := range untrusted {
		rec := tc.request.serve(router)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a key: status = %d, want 401; body = %s", name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "missing management key") {
			t.Fatalf("%s: body = %s, want the normal missing-key error", name, rec.Body.String())
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("%s: untrusted responses keep today's CORS headers, got %q", name, got)
		}
		withKey := tc.request.with("X-CPA-Keyless", "").with("X-Management-Key", tailnetTestKey).serve(router)
		if withKey.Code != http.StatusOK {
			t.Fatalf("%s with the key: status = %d, want 200; body = %s", name, withKey.Code, withKey.Body.String())
		}
		wantCORS := "*"
		if tc.trustedWithKey {
			wantCORS = ""
		}
		if got := withKey.Header().Get("Access-Control-Allow-Origin"); got != wantCORS {
			t.Fatalf("%s with the key: Access-Control-Allow-Origin = %q, want %q", name, got, wantCORS)
		}
	}
	if failures := h.tailnetTestFailures(); failures != 0 {
		t.Fatalf("missing keys counted toward the ban: %d", failures)
	}
}

func TestTailnetAuthMiddleware_DisabledChangesNothing(t *testing.T) {
	h := newTailnetTestHandler(t, func(cfg *config.Config) { cfg.RemoteManagement.TailnetAuth.Enabled = false })
	router := newTailnetTestRouter(t, h)
	for _, request := range []tailnetTestRequest{tailnetTrusted("/v8/management/config"), tailnetLocal("/v0/management/auth-files")} {
		if rec := request.serve(router); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with tailnet-auth off: status = %d, want 401", request.path, rec.Code)
		}
		if rec := request.with("Authorization", "Bearer "+tailnetTestKey).serve(router); rec.Code != http.StatusOK {
			t.Fatalf("%s with the key and tailnet-auth off: status = %d, want 200", request.path, rec.Code)
		}
	}
}

func TestTailnetAuthMiddleware_AllowRemoteOffLimitsTrustToLoopbackClients(t *testing.T) {
	h := newTailnetTestHandler(t, func(cfg *config.Config) { cfg.RemoteManagement.AllowRemote = false })
	router := newTailnetTestRouter(t, h)

	rec := tailnetTrusted("/v8/management/config").serve(router)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "remote management disabled") {
		t.Fatalf("tailnet request with allow-remote off: status = %d body = %s, want 403 remote management disabled", rec.Code, rec.Body.String())
	}
	native := tailnetLocal("/v8/management/config").withPeer("127.0.0.1:50000") // CPA running natively, not in Docker
	if rec := native.serve(router); rec.Code != http.StatusOK {
		t.Fatalf("loopback client with allow-remote off: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestTailnetAuthMiddleware_NeverOpensAManagementAPIWithoutAKey(t *testing.T) {
	h := newTailnetTestHandler(t, func(cfg *config.Config) { cfg.RemoteManagement.SecretKey = "" })
	h.localPassword = "tui-local-password"
	router := newTailnetTestRouter(t, h)
	for _, request := range []tailnetTestRequest{tailnetTrusted("/v8/management/config"), tailnetLocal("/v8/management/config")} {
		rec := request.serve(router)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "remote management key not set") {
			t.Fatalf("%s without any management key configured: status = %d body = %s, want 403", request.host, rec.Code, rec.Body.String())
		}
	}
	h.envSecret = "env-management-password"
	if rec := tailnetTrusted("/v8/management/config").serve(router); rec.Code != http.StatusOK {
		t.Fatalf("trusted request with MANAGEMENT_PASSWORD set: status = %d, want 200", rec.Code)
	}
}

func TestTailnetAuthMiddleware_TrustWinsOverABannedClientIP(t *testing.T) {
	h := newTailnetTestHandler(t, nil)
	router := newTailnetTestRouter(t, h)

	// Five wrong keys from the device in an untrusted shape (cross-origin) ban its IP.
	wrong := tailnetTrusted("/v8/management/config").with("Origin", "http://evil.example").with("Authorization", "Bearer wrong")
	for i := 0; i < 5; i++ {
		if rec := wrong.serve(router); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong key %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := wrong.with("Authorization", "Bearer "+tailnetTestKey).serve(router); rec.Code != http.StatusForbidden {
		t.Fatalf("correct key from the banned IP: status = %d, want 403", rec.Code)
	}
	if rec := tailnetTrusted("/v8/management/config").serve(router); rec.Code != http.StatusOK {
		t.Fatalf("trusted request from the banned IP: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAuthenticateManagementKey_MissingKeyNeverCountsTowardTheBan(t *testing.T) {
	h := newTailnetTestHandler(t, func(cfg *config.Config) { cfg.RemoteManagement.TailnetAuth = config.TailnetAuthConfig{} })
	router := newTailnetTestRouter(t, h)

	probe := tailnetTrusted("/v8/management/auth/session")
	for i := 0; i < 10; i++ {
		rec := probe.serve(router)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "missing management key") {
			t.Fatalf("probe %d: status = %d body = %s, want 401 missing management key", i+1, rec.Code, rec.Body.String())
		}
	}
	for i := 0; i < 10; i++ {
		allowed, status, message := h.AuthenticateManagementKey(tailnetTestDevice, false, "")
		if allowed || status != http.StatusUnauthorized || message != "missing management key" {
			t.Fatalf("direct call %d: allowed=%v status=%d message=%q", i+1, allowed, status, message)
		}
	}
	if failures := h.tailnetTestFailures(); failures != 0 {
		t.Fatalf("missing keys counted toward the ban: %d", failures)
	}
	rec := probe.with("Authorization", "Bearer "+tailnetTestKey).serve(router)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct key after 20 credential-less requests: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var session authSessionResponse
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &session); errDecode != nil || session.Method != tailnetauth.MethodKey {
		t.Fatalf("session = %+v (%v), want method key", session, errDecode)
	}
}

func TestAuthenticateManagementKey_BannedIPGetsForbiddenEvenWithoutAKey(t *testing.T) {
	h := newTailnetTestHandler(t, nil)
	for i := 0; i < 5; i++ {
		h.AuthenticateManagementKey(tailnetTestDevice, false, "wrong")
	}
	allowed, status, message := h.AuthenticateManagementKey(tailnetTestDevice, false, "")
	if allowed || status != http.StatusForbidden || !strings.HasPrefix(message, "IP banned") {
		t.Fatalf("missing key from a banned IP: allowed=%v status=%d message=%q, want 403 banned", allowed, status, message)
	}
}

func TestGetAuthSession_ReportsHowTheRequestAuthenticated(t *testing.T) {
	h := newTailnetTestHandler(t, nil)
	router := newTailnetTestRouter(t, h)
	const path = "/v8/management/auth/session"

	cases := []struct {
		name    string
		request tailnetTestRequest
		want    authSessionResponse
	}{
		{"tailnet", tailnetTrusted(path), authSessionResponse{Authenticated: true, Method: "tailnet", Login: tailnetTestLogin, Device: tailnetTestDevice}},
		{"tagged device", tailnetTrusted(path).with("Tailscale-User-Login", "").with("Tailscale-User-Name", ""), authSessionResponse{Authenticated: true, Method: "tailnet", Device: tailnetTestDevice}},
		{"local", tailnetLocal(path), authSessionResponse{Authenticated: true, Method: "local"}},
		{"key", tailnetTrusted(path).with("X-Forwarded-For", tailnetTestUnlisted).with("Authorization", "Bearer "+tailnetTestKey), authSessionResponse{Authenticated: true, Method: "key"}},
	}
	for _, tc := range cases {
		rec := tc.request.serve(router)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body = %s", tc.name, rec.Code, rec.Body.String())
		}
		var raw map[string]any
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &raw); errDecode != nil {
			t.Fatalf("%s: body is not JSON: %v", tc.name, errDecode)
		}
		for _, key := range []string{"authenticated", "method", "login", "device"} {
			if _, ok := raw[key]; !ok {
				t.Fatalf("%s: body %s lacks %q", tc.name, rec.Body.String(), key)
			}
		}
		var got authSessionResponse
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &got); errDecode != nil || got != tc.want {
			t.Fatalf("%s: session = %+v, want %+v", tc.name, got, tc.want)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", tc.name, got)
		}
	}

	rec := tailnetTrusted(path).with("X-Forwarded-For", tailnetTestUnlisted).serve(router)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted probe without a key: status = %d, want 401", rec.Code)
	}
	if failures := h.tailnetTestFailures(); failures != 0 {
		t.Fatalf("session probes counted toward the ban: %d", failures)
	}
}

// The tailnet-auth block must round-trip through the real /v8 config write path
// (lists replaced whole, explicit false kept), take effect for the very next
// management request, and survive a later v0 save of an unrelated field.
func TestTailnetAuthConfig_V8WritePathRoundTripsAndTakesEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# Operator comment\nconfig-version: 8\nserver:\n  port: 8317\n  trusted-proxies:\n    - 172.16.0.0/12\nmanagement:\n  allow-remote: true\n  secret-key: \"" + tailnetTestHash(t) + "\"\n"
	if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := &Handler{cfg: cfg, configFilePath: path, failedAttempts: make(map[string]*attemptInfo)}
	router := newTailnetTestRouter(t, h)
	keyed := router.Group("/v8/management", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		keyed.Handle(method, "/config/*path", h.ConfigV8)
	}
	router.PUT("/v0/management/debug", h.PutDebug)
	call := func(method, url, body string, status int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
		if rec.Code != status {
			t.Fatalf("%s %s: status = %d, want %d; body = %s", method, url, rec.Code, status, rec.Body.String())
		}
		return rec.Body.String()
	}

	// A config rendered before this change has no block: the GET is a 404.
	call(http.MethodGet, "/v8/management/config/management/tailnet-auth", "", http.StatusNotFound)
	if rec := tailnetTrusted("/v8/management/auth/session").serve(router); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session before the write: status = %d, want 401", rec.Code)
	}

	block := `{"enabled":true,"allowed-logins":["` + tailnetTestLogin + `"],"allowed-devices":["` + tailnetTestDevice + `"],` +
		`"allowed-hosts":["devbox:8318","devbox.example-tailnet.ts.net:8318","localhost","127.0.0.1"],"allow-local":false,"proxy-api":true}`
	call(http.MethodPut, "/v8/management/config/management/tailnet-auth", block, http.StatusOK)

	var view map[string]any
	if errDecode := json.Unmarshal([]byte(call(http.MethodGet, "/v8/management/config/management/tailnet-auth", "", http.StatusOK)), &view); errDecode != nil {
		t.Fatal(errDecode)
	}
	var want map[string]any
	if errDecode := json.Unmarshal([]byte(block), &want); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !tailnetJSONEqual(view, want) {
		t.Fatalf("GET tailnet-auth = %v, want %v", view, want)
	}
	// The in-memory config changed with the write: the device is trusted at once.
	if rec := tailnetTrusted("/v8/management/auth/session").serve(router); rec.Code != http.StatusOK {
		t.Fatalf("session after the write: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rec := tailnetLocal("/v8/management/auth/session").serve(router); rec.Code != http.StatusUnauthorized {
		t.Fatalf("local session with allow-local false: status = %d, want 401", rec.Code)
	}

	// A legacy v0 save of an unrelated field keeps the block, explicit false included.
	call(http.MethodPut, "/v0/management/debug", `{"value":true}`, http.StatusOK)
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if errValidate := config.ValidateV8Config(saved); errValidate != nil {
		t.Fatalf("saved config is not valid v8: %v\n%s", errValidate, saved)
	}
	text := string(saved)
	if !strings.Contains(text, "# Operator comment") || !strings.Contains(text, "allow-local: false") || strings.Contains(text, "remote-management") {
		t.Fatalf("v0 save lost the comment, the explicit false or the v8 layout:\n%s", text)
	}
	reloaded, errReload := config.LoadConfig(path)
	if errReload != nil {
		t.Fatal(errReload)
	}
	got := reloaded.RemoteManagement.TailnetAuth
	if !got.Enabled || got.AllowLocal || !got.ProxyAPI || len(got.AllowedLogins) != 1 || len(got.AllowedDevices) != 1 || len(got.AllowedHosts) != 4 {
		t.Fatalf("reloaded tailnet-auth = %+v", got)
	}

	// Lists are replaced whole; an empty list stays explicit and fails closed.
	call(http.MethodPut, "/v8/management/config/management/tailnet-auth/allowed-devices", `[]`, http.StatusOK)
	if rec := tailnetTrusted("/v8/management/auth/session").serve(router); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session after emptying allowed-devices: status = %d, want 401", rec.Code)
	}
	if saved, _ = os.ReadFile(path); !strings.Contains(string(saved), "allowed-devices: []") {
		t.Fatalf("explicit empty allowed-devices was not kept:\n%s", saved)
	}

	// Unknown keys are rejected rather than silently ignored.
	call(http.MethodPatch, "/v8/management/config/management/tailnet-auth", `{"allowed-device":["100.64.0.11"]}`, http.StatusBadRequest)
}

func tailnetJSONEqual(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}
