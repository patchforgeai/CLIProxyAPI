package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	usageTestClaudeUsagePath   = "/api/oauth/usage"
	usageTestClaudeProfilePath = "/api/oauth/profile"
	usageTestCodexUsagePath    = "/backend-api/wham/usage"
	usageTestCodexCreditsPath  = "/backend-api/wham/rate-limit-reset-credits"
)

// usageTestStart is the fixed start of every fake clock in these tests.
var usageTestStart = time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)

// usageTestReceive returns the next value of ch. The timeout only turns a
// regression that would hang into a failure; no test relies on it for timing.
func usageTestReceive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// usageTestWaitGroup waits for wg like usageTestReceive.
func usageTestWaitGroup(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	usageTestReceive(t, done, what)
}

// usageTestClock is a controllable clock, safe for concurrent use.
type usageTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newUsageTestClock() *usageTestClock {
	return &usageTestClock{now: usageTestStart}
}

func (c *usageTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *usageTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// usageTestResponse is how the fake upstream answers one path.
type usageTestResponse struct {
	status int
	body   string
	// drop closes the connection without a response: a transport error.
	drop bool
	// truncate announces a longer body than it sends: a body read error.
	truncate bool
}

// usageTestUpstream stands in for the provider usage endpoints. It counts
// calls and records request headers per path.
type usageTestUpstream struct {
	*httptest.Server

	mu        sync.Mutex
	responses map[string]usageTestResponse
	calls     map[string]int
	headers   map[string][]http.Header
	// gate, when set, holds every request until it is closed; arrived gets a
	// value as each request comes in.
	gate    chan struct{}
	arrived chan struct{}
}

func newUsageTestUpstream(t *testing.T) *usageTestUpstream {
	t.Helper()
	upstream := &usageTestUpstream{
		responses: make(map[string]usageTestResponse),
		calls:     make(map[string]int),
		headers:   make(map[string][]http.Header),
	}
	upstream.Server = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *usageTestUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.calls[r.URL.Path]++
	u.headers[r.URL.Path] = append(u.headers[r.URL.Path], r.Header.Clone())
	resp, ok := u.responses[r.URL.Path]
	gate, arrived := u.gate, u.arrived
	u.mu.Unlock()
	if arrived != nil {
		arrived <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	if !ok {
		resp = usageTestResponse{status: http.StatusNotFound, body: `{"error":"not found"}`}
	}
	switch {
	case resp.drop:
		if conn, _, errHijack := w.(http.Hijacker).Hijack(); errHijack == nil {
			_ = conn.Close()
		}
	case resp.truncate:
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
		w.(http.Flusher).Flush()
		if conn, _, errHijack := w.(http.Hijacker).Hijack(); errHijack == nil {
			_ = conn.Close()
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}
}

func (u *usageTestUpstream) respond(path string, resp usageTestResponse) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.responses[path] = resp
}

func (u *usageTestUpstream) respondJSON(path string, status int, body string) {
	u.respond(path, usageTestResponse{status: status, body: body})
}

func (u *usageTestUpstream) callCount(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[path]
}

func (u *usageTestUpstream) requestHeaders(path string) []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.headers[path]...)
}

func (u *usageTestUpstream) hold(gate, arrived chan struct{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gate, u.arrived = gate, arrived
}

// usageTestTargets maps the fake upstream's paths to the production endpoints,
// so bodies are still parsed as Claude and Codex usage.
func usageTestTargets(baseURL string) map[string]usageTarget {
	return map[string]usageTarget{
		baseURL + usageTestClaudeUsagePath:   {endpoint: usageEndpointClaudeUsage, canonicalURL: quotareading.ClaudeUsageURL},
		baseURL + usageTestClaudeProfilePath: {endpoint: usageEndpointClaudeProfile, canonicalURL: quotareading.ClaudeProfileURL},
		baseURL + usageTestCodexUsagePath:    {endpoint: usageEndpointCodexUsage, canonicalURL: quotareading.CodexUsageURL},
	}
}

// installUsageCache gives h a cache built by the test.
func installUsageCache(t *testing.T, h *Handler, cache *usageCache) {
	t.Helper()
	devyreStates.Store(h, &devyreHandlerState{usage: cache})
	t.Cleanup(func() { devyreStates.Delete(h) })
}

// usageTestIDToken builds an unsigned Codex id_token carrying accountID.
func usageTestIDToken(accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
}

func usageTestClaudeAuth(id, token string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		Metadata: map[string]any{"type": "claude", "access_token": token},
	}
}

func usageTestCodexAuth(id, token, accountID string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "access_token": token, "id_token": usageTestIDToken(accountID)},
	}
}

// usageCacheHarness wires a management handler with a fake upstream, a fake
// clock and a private readings store.
type usageCacheHarness struct {
	clock    *usageTestClock
	upstream *usageTestUpstream
	cache    *usageCache
	readings *quotareading.Store
	handler  *Handler
	manager  *coreauth.Manager
	router   *gin.Engine
	claude   *coreauth.Auth
	codex    *coreauth.Auth
}

func newUsageCacheHarness(t *testing.T, routing config.RoutingConfig) *usageCacheHarness {
	t.Helper()
	hs := &usageCacheHarness{
		clock:    newUsageTestClock(),
		upstream: newUsageTestUpstream(t),
		readings: quotareading.NewStore(),
		manager:  coreauth.NewManager(nil, nil, nil),
	}
	hs.cache = &usageCache{nowFunc: hs.clock.Now, targets: usageTestTargets(hs.upstream.URL), readings: hs.readings}
	hs.handler = &Handler{cfg: &config.Config{Routing: routing}, authManager: hs.manager}
	installUsageCache(t, hs.handler, hs.cache)
	hs.claude = hs.register(t, usageTestClaudeAuth("claude-a.json", "claude-a-token"))
	hs.codex = hs.register(t, usageTestCodexAuth("codex-c.json", "codex-c-token", "acc-codex-c"))
	hs.router = gin.New()
	hs.router.POST("/v0/management/api-call", hs.handler.APICall)
	hs.router.POST("/v8/management/requests/api-call", hs.handler.APICall)
	return hs
}

func (hs *usageCacheHarness) register(t *testing.T, auth *coreauth.Auth) *coreauth.Auth {
	t.Helper()
	if _, errRegister := hs.manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register %s: %v", auth.ID, errRegister)
	}
	auth.EnsureIndex()
	return auth
}

func (hs *usageCacheHarness) url(path string) string {
	return hs.upstream.URL + path
}

// usageTestCall is one management api-call.
type usageTestCall struct {
	route     string
	authIndex string
	method    string
	url       string
	header    map[string]string
	refresh   bool
}

// usageTestResult is the management answer to one api-call.
type usageTestResult struct {
	code int
	raw  string
	resp apiCallResponse
}

func (r usageTestResult) label() string {
	return usageCacheLabel(r.resp)
}

func (hs *usageCacheHarness) do(call usageTestCall) (usageTestResult, error) {
	route := call.route
	if route == "" {
		route = "/v0/management/api-call"
	}
	header := call.header
	if header == nil && call.authIndex != "" {
		header = map[string]string{"Authorization": "Bearer $TOKEN$"}
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"auth_index": call.authIndex,
		"method":     call.method,
		"url":        call.url,
		"header":     header,
	})
	if errMarshal != nil {
		return usageTestResult{}, errMarshal
	}
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	if call.refresh {
		req.Header.Set(usageCacheHeader, usageCacheRefreshValue)
	}
	recorder := httptest.NewRecorder()
	hs.router.ServeHTTP(recorder, req)
	result := usageTestResult{code: recorder.Code, raw: recorder.Body.String()}
	if recorder.Code == http.StatusOK {
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &result.resp); errDecode != nil {
			return result, fmt.Errorf("decode api-call response %q: %w", result.raw, errDecode)
		}
	}
	return result, nil
}

func (hs *usageCacheHarness) mustDo(t *testing.T, call usageTestCall) usageTestResult {
	t.Helper()
	result, errDo := hs.do(call)
	if errDo != nil {
		t.Fatal(errDo)
	}
	return result
}

// get issues an api-call GET of path for auth.
func (hs *usageCacheHarness) get(t *testing.T, auth *coreauth.Auth, path string) usageTestResult {
	t.Helper()
	return hs.mustDo(t, usageTestCall{authIndex: auth.Index, method: http.MethodGet, url: hs.url(path)})
}

// refresh issues an api-call GET of path for auth that asks for a cache bypass.
func (hs *usageCacheHarness) refresh(t *testing.T, auth *coreauth.Auth, path string) usageTestResult {
	t.Helper()
	return hs.mustDo(t, usageTestCall{authIndex: auth.Index, method: http.MethodGet, url: hs.url(path), refresh: true})
}

// expectUpstream asserts the api-call result and the upstream call count.
func expectUpstream(t *testing.T, step string, hs *usageCacheHarness, path string, result usageTestResult, status int, body, label string, calls int) {
	t.Helper()
	if result.code != http.StatusOK {
		t.Fatalf("%s: management status = %d, want 200; body = %s", step, result.code, result.raw)
	}
	if result.resp.StatusCode != status || result.resp.Body != body || result.label() != label {
		t.Fatalf("%s: got status=%d body=%q label=%q, want status=%d body=%q label=%q",
			step, result.resp.StatusCode, result.resp.Body, result.label(), status, body, label)
	}
	if got := hs.upstream.callCount(path); got != calls {
		t.Fatalf("%s: upstream calls = %d, want %d", step, got, calls)
	}
}

func TestUsageCacheServesFreshResponsesUntilTTL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		codex   bool
		routing config.RoutingConfig
		ttl     time.Duration
	}{
		{name: "claude usage default", path: usageTestClaudeUsagePath, ttl: 5 * time.Minute},
		{name: "claude usage configured", path: usageTestClaudeUsagePath, ttl: 2 * time.Minute, routing: config.RoutingConfig{
			QuotaObservation: config.QuotaObservationConfig{UsageCache: config.UsageCacheConfig{ClaudeUsageTTL: "2m"}},
		}},
		{name: "codex usage default", path: usageTestCodexUsagePath, codex: true, ttl: time.Minute},
		{name: "codex usage configured", path: usageTestCodexUsagePath, codex: true, ttl: 90 * time.Second, routing: config.RoutingConfig{
			QuotaObservation: config.QuotaObservationConfig{UsageCache: config.UsageCacheConfig{CodexUsageTTL: "90s"}},
		}},
		{name: "claude profile", path: usageTestClaudeProfilePath, ttl: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs := newUsageCacheHarness(t, tc.routing)
			auth := hs.claude
			if tc.codex {
				auth = hs.codex
			}
			hs.upstream.respondJSON(tc.path, http.StatusOK, `{"n":1}`)
			expectUpstream(t, "first", hs, tc.path, hs.get(t, auth, tc.path), http.StatusOK, `{"n":1}`, usageCacheMiss, 1)

			hs.upstream.respondJSON(tc.path, http.StatusOK, `{"n":2}`)
			hs.clock.Advance(tc.ttl - time.Second)
			expectUpstream(t, "inside ttl", hs, tc.path, hs.get(t, auth, tc.path), http.StatusOK, `{"n":1}`, usageCacheHit, 1)

			hs.clock.Advance(time.Second)
			expectUpstream(t, "at ttl", hs, tc.path, hs.get(t, auth, tc.path), http.StatusOK, `{"n":2}`, usageCacheMiss, 2)
		})
	}
}

func TestUsageCacheHitSkipsTokenResolution(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, `{"n":1}`)
	expectUpstream(t, "miss", hs, usageTestClaudeUsagePath, hs.get(t, hs.claude, usageTestClaudeUsagePath), http.StatusOK, `{"n":1}`, usageCacheMiss, 1)

	// Without a token an uncached call fails before reaching upstream.
	tokenless := hs.claude.Clone()
	delete(tokenless.Metadata, "access_token")
	if _, errUpdate := hs.manager.Update(context.Background(), tokenless); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	expectUpstream(t, "hit", hs, usageTestClaudeUsagePath, hs.get(t, hs.claude, usageTestClaudeUsagePath), http.StatusOK, `{"n":1}`, usageCacheHit, 1)

	hs.clock.Advance(5 * time.Minute)
	expired := hs.get(t, hs.claude, usageTestClaudeUsagePath)
	if expired.code != http.StatusBadRequest || !strings.Contains(expired.raw, "auth token not found") {
		t.Fatalf("expired: status=%d body=%s, want 400 auth token not found", expired.code, expired.raw)
	}
	if got := hs.upstream.callCount(usageTestClaudeUsagePath); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}

	// The early return released the key: nothing is left in flight and the
	// next request, with the token back, leads a new upstream call.
	hs.cache.mu.Lock()
	inFlight := hs.cache.entries[usageCacheKey(hs.claude.Index, hs.url(usageTestClaudeUsagePath))].flight
	hs.cache.mu.Unlock()
	if inFlight != nil {
		t.Fatal("an api-call that returned early left its upstream call in flight")
	}
	if _, errUpdate := hs.manager.Update(context.Background(), hs.claude.Clone()); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, `{"n":2}`)
	expectUpstream(t, "token restored", hs, usageTestClaudeUsagePath, hs.get(t, hs.claude, usageTestClaudeUsagePath), http.StatusOK, `{"n":2}`, usageCacheMiss, 2)
}

func TestUsageCacheCoalescesConcurrentRequests(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, `{"n":1}`)
	gate, arrived := make(chan struct{}), make(chan struct{}, 10)
	hs.upstream.hold(gate, arrived)
	// Registered after the harness, so it runs before the upstream closes,
	// which waits for held requests.
	openGate := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(openGate)
	var waiting sync.WaitGroup
	waiting.Add(9)
	hs.cache.testHookFollowerWaiting = waiting.Done

	results := make([]usageTestResult, 10)
	errs := make([]error, 10)
	var finished sync.WaitGroup
	for i := range results {
		finished.Add(1)
		go func() {
			defer finished.Done()
			results[i], errs[i] = hs.do(usageTestCall{authIndex: hs.claude.Index, method: http.MethodGet, url: hs.url(usageTestClaudeUsagePath)})
		}()
	}
	usageTestReceive(t, arrived, "the leader to reach upstream")
	usageTestWaitGroup(t, &waiting, "nine followers to wait for the leader")
	openGate()
	usageTestWaitGroup(t, &finished, "all requests to finish")

	for i, result := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if result.code != http.StatusOK || result.resp.StatusCode != http.StatusOK || result.resp.Body != `{"n":1}` || result.label() != usageCacheMiss {
			t.Fatalf("request %d: status=%d upstream=%d body=%q label=%q", i, result.code, result.resp.StatusCode, result.resp.Body, result.label())
		}
	}
	if got := hs.upstream.callCount(usageTestClaudeUsagePath); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestUsageCacheServesStaleOnUpstreamErrors(t *testing.T) {
	t.Parallel()
	failures := []struct {
		name string
		resp usageTestResponse
	}{
		{name: "500", resp: usageTestResponse{status: http.StatusInternalServerError, body: `{"error":"boom"}`}},
		{name: "503", resp: usageTestResponse{status: http.StatusServiceUnavailable}},
		{name: "429 without backoff", resp: usageTestResponse{status: http.StatusTooManyRequests}},
		{name: "transport error", resp: usageTestResponse{drop: true}},
		{name: "body read error", resp: usageTestResponse{truncate: true}},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs := newUsageCacheHarness(t, config.RoutingConfig{})
			path := usageTestCodexUsagePath
			hs.upstream.respondJSON(path, http.StatusOK, `{"n":1}`)
			expectUpstream(t, "miss", hs, path, hs.get(t, hs.codex, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 1)

			hs.clock.Advance(time.Minute)
			hs.upstream.respond(path, tc.resp)
			expectUpstream(t, "failure", hs, path, hs.get(t, hs.codex, path), http.StatusOK, `{"n":1}`, usageCacheStale, 2)
			// Codex has no backoff: the next request tries upstream again.
			expectUpstream(t, "failure again", hs, path, hs.get(t, hs.codex, path), http.StatusOK, `{"n":1}`, usageCacheStale, 3)

			hs.upstream.respondJSON(path, http.StatusOK, `{"n":2}`)
			expectUpstream(t, "recovered", hs, path, hs.get(t, hs.codex, path), http.StatusOK, `{"n":2}`, usageCacheMiss, 4)
		})
	}
}

func TestUsageCachePassesErrorsThroughWithoutCachedResponse(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	path := usageTestCodexUsagePath

	hs.upstream.respondJSON(path, http.StatusInternalServerError, `{"error":"boom"}`)
	expectUpstream(t, "500", hs, path, hs.get(t, hs.codex, path), http.StatusInternalServerError, `{"error":"boom"}`, usageCacheMiss, 1)

	hs.upstream.respondJSON(path, http.StatusTooManyRequests, `{"error":"slow down"}`)
	expectUpstream(t, "codex 429", hs, path, hs.get(t, hs.codex, path), http.StatusTooManyRequests, `{"error":"slow down"}`, usageCacheMiss, 2)
	expectUpstream(t, "codex 429 again", hs, path, hs.get(t, hs.codex, path), http.StatusTooManyRequests, `{"error":"slow down"}`, usageCacheMiss, 3)

	hs.upstream.respondJSON(path, http.StatusUnauthorized, `{"error":"expired"}`)
	expectUpstream(t, "401", hs, path, hs.get(t, hs.codex, path), http.StatusUnauthorized, `{"error":"expired"}`, usageCacheMiss, 4)

	hs.upstream.respond(path, usageTestResponse{drop: true})
	dropped := hs.get(t, hs.codex, path)
	if dropped.code != http.StatusBadGateway || !strings.Contains(dropped.raw, "request failed") {
		t.Fatalf("transport error: status=%d body=%s, want 502 request failed", dropped.code, dropped.raw)
	}

	hs.upstream.respond(path, usageTestResponse{truncate: true})
	truncated := hs.get(t, hs.codex, path)
	if truncated.code != http.StatusBadGateway || !strings.Contains(truncated.raw, "failed to read response") {
		t.Fatalf("body read error: status=%d body=%s, want 502 failed to read response", truncated.code, truncated.raw)
	}
}

func TestUsageCacheBacksOffClaudeUsageAfter429(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	path := usageTestClaudeUsagePath
	hs.upstream.respondJSON(path, http.StatusOK, `{"n":1}`)
	expectUpstream(t, "miss", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 1)

	hs.clock.Advance(5 * time.Minute)
	hs.upstream.respondJSON(path, http.StatusTooManyRequests, `{"error":"rate_limited"}`)
	calls := 2
	expectUpstream(t, "first 429", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheStale, calls)

	// Each backoff ends with another 429, which doubles it up to the 60 minute cap.
	for _, backoff := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 60 * time.Minute, 60 * time.Minute} {
		step := fmt.Sprintf("backoff %s", backoff)
		hs.clock.Advance(backoff - time.Second)
		expectUpstream(t, step+" before end", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheStale, calls)
		expectUpstream(t, step+" refresh before end", hs, path, hs.refresh(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheStale, calls)
		hs.clock.Advance(time.Second)
		calls++
		expectUpstream(t, step+" at end", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheStale, calls)
	}

	// A 2xx clears the backoff; the next 429 starts again at 5 minutes.
	hs.clock.Advance(60 * time.Minute)
	hs.upstream.respondJSON(path, http.StatusOK, `{"n":2}`)
	calls++
	expectUpstream(t, "recovered", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheMiss, calls)
	hs.clock.Advance(5 * time.Minute)
	hs.upstream.respondJSON(path, http.StatusTooManyRequests, `{"error":"rate_limited"}`)
	calls++
	expectUpstream(t, "429 after reset", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheStale, calls)
	hs.clock.Advance(5*time.Minute - time.Second)
	expectUpstream(t, "reset backoff before end", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheStale, calls)
	hs.clock.Advance(time.Second)
	calls++
	expectUpstream(t, "reset backoff at end", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheStale, calls)
}

func TestUsageCacheServesStored429DuringBackoff(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	path := usageTestClaudeUsagePath
	hs.upstream.respondJSON(path, http.StatusTooManyRequests, `{"error":"rate_limited"}`)
	expectUpstream(t, "429", hs, path, hs.get(t, hs.claude, path), http.StatusTooManyRequests, `{"error":"rate_limited"}`, usageCacheMiss, 1)
	expectUpstream(t, "backoff", hs, path, hs.get(t, hs.claude, path), http.StatusTooManyRequests, `{"error":"rate_limited"}`, usageCacheBackoff, 1)
	expectUpstream(t, "refresh in backoff", hs, path, hs.refresh(t, hs.claude, path), http.StatusTooManyRequests, `{"error":"rate_limited"}`, usageCacheBackoff, 1)

	// Backoff is per credential: another Claude credential is not held back.
	other := hs.register(t, usageTestClaudeAuth("claude-b.json", "claude-b-token"))
	hs.upstream.respondJSON(path, http.StatusOK, `{"n":1}`)
	expectUpstream(t, "other credential", hs, path, hs.get(t, other, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 2)

	hs.clock.Advance(5 * time.Minute)
	expectUpstream(t, "after backoff", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 3)
}

func TestUsageCacheRefreshBypassHonorsFloor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		routing config.RoutingConfig
		floor   time.Duration
	}{
		{name: "default floor", floor: 30 * time.Second},
		{name: "configured floor", floor: time.Minute, routing: config.RoutingConfig{
			QuotaObservation: config.QuotaObservationConfig{UsageCache: config.UsageCacheConfig{RefreshFloor: "1m"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hs := newUsageCacheHarness(t, tc.routing)
			path := usageTestClaudeUsagePath
			hs.upstream.respondJSON(path, http.StatusOK, `{"n":1}`)
			expectUpstream(t, "miss", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 1)

			hs.upstream.respondJSON(path, http.StatusOK, `{"n":2}`)
			hs.clock.Advance(tc.floor - time.Second)
			expectUpstream(t, "refresh inside floor", hs, path, hs.refresh(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheHit, 1)
			hs.clock.Advance(time.Second)
			expectUpstream(t, "refresh at floor", hs, path, hs.refresh(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheBypass, 2)
			expectUpstream(t, "plain after bypass", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheHit, 2)

			// The floor counts from the newest upstream call, the bypass itself.
			hs.upstream.respondJSON(path, http.StatusOK, `{"n":3}`)
			hs.clock.Advance(tc.floor - time.Second)
			expectUpstream(t, "second refresh inside floor", hs, path, hs.refresh(t, hs.claude, path), http.StatusOK, `{"n":2}`, usageCacheHit, 2)
			hs.clock.Advance(time.Second)
			expectUpstream(t, "second refresh at floor", hs, path, hs.refresh(t, hs.claude, path), http.StatusOK, `{"n":3}`, usageCacheBypass, 3)
		})
	}
}

func TestUsageCacheNeverCachesPostsOrOtherURLs(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, `{"n":1}`)
	hs.upstream.respondJSON(usageTestCodexCreditsPath, http.StatusOK, `{"available_count":1}`)
	hs.upstream.respondJSON(usageTestCodexCreditsPath+"/consume", http.StatusOK, `{"ok":true}`)

	calls := []struct {
		name string
		path string
		call usageTestCall
	}{
		{name: "POST usage URL", path: usageTestClaudeUsagePath, call: usageTestCall{authIndex: hs.claude.Index, method: http.MethodPost, url: hs.url(usageTestClaudeUsagePath)}},
		{name: "GET credits URL", path: usageTestCodexCreditsPath, call: usageTestCall{authIndex: hs.codex.Index, method: http.MethodGet, url: hs.url(usageTestCodexCreditsPath)}},
		{name: "POST credits consume", path: usageTestCodexCreditsPath + "/consume", call: usageTestCall{authIndex: hs.codex.Index, method: http.MethodPost, url: hs.url(usageTestCodexCreditsPath + "/consume")}},
		{name: "GET usage URL with query", path: usageTestClaudeUsagePath, call: usageTestCall{authIndex: hs.claude.Index, method: http.MethodGet, url: hs.url(usageTestClaudeUsagePath) + "?cedar_ember=1"}},
		{name: "GET usage URL without auth index", path: usageTestClaudeUsagePath, call: usageTestCall{method: http.MethodGet, url: hs.url(usageTestClaudeUsagePath)}},
	}
	for _, tc := range calls {
		before := hs.upstream.callCount(tc.path)
		for i := range 2 {
			result := hs.mustDo(t, tc.call)
			if result.code != http.StatusOK || result.label() != "" {
				t.Fatalf("%s #%d: status=%d label=%q, want 200 without annotation", tc.name, i, result.code, result.label())
			}
		}
		if got := hs.upstream.callCount(tc.path) - before; got != 2 {
			t.Fatalf("%s: upstream calls = %d, want 2", tc.name, got)
		}
	}
}

func TestDefaultUsageTargetsAllowlistExactUsageURLs(t *testing.T) {
	t.Parallel()
	want := map[string]usageTarget{
		"https://api.anthropic.com/api/oauth/usage":   {endpoint: usageEndpointClaudeUsage, canonicalURL: quotareading.ClaudeUsageURL},
		"https://api.anthropic.com/api/oauth/profile": {endpoint: usageEndpointClaudeProfile, canonicalURL: quotareading.ClaudeProfileURL},
		"https://chatgpt.com/backend-api/wham/usage":  {endpoint: usageEndpointCodexUsage, canonicalURL: quotareading.CodexUsageURL},
	}
	got := defaultUsageTargets()
	if len(got) != len(want) {
		t.Fatalf("allowlist = %v, want %v", got, want)
	}
	for rawURL, target := range want {
		if got[rawURL] != target {
			t.Fatalf("allowlist[%q] = %+v, want %+v", rawURL, got[rawURL], target)
		}
	}
	for _, rawURL := range []string{
		"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits",
		"https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume",
		"https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1",
		"https://chatgpt.com/backend-api/wham/usage/",
	} {
		if _, ok := got[rawURL]; ok {
			t.Fatalf("allowlist must not contain %q", rawURL)
		}
	}
}

func TestUsageCacheDisabledPassesThrough(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{
		QuotaObservation: config.QuotaObservationConfig{UsageCache: config.UsageCacheConfig{Enabled: new(false)}},
	})
	path := usageTestClaudeUsagePath
	hs.upstream.respondJSON(path, http.StatusOK, usageTestClaudeBody)
	expectUpstream(t, "first", hs, path, hs.get(t, hs.claude, path), http.StatusOK, usageTestClaudeBody, "", 1)
	expectUpstream(t, "second", hs, path, hs.get(t, hs.claude, path), http.StatusOK, usageTestClaudeBody, "", 2)
	expectUpstream(t, "refresh", hs, path, hs.refresh(t, hs.claude, path), http.StatusOK, usageTestClaudeBody, "", 3)

	// The readings feed is not part of the cache and keeps working.
	if windows := hs.readings.Get(hs.claude.ID).Windows; len(windows) != 2 {
		t.Fatalf("readings = %+v, want the 5h and 7d windows", windows)
	}

	hs.upstream.respondJSON(path, http.StatusTooManyRequests, `{"error":"rate_limited"}`)
	expectUpstream(t, "429", hs, path, hs.get(t, hs.claude, path), http.StatusTooManyRequests, `{"error":"rate_limited"}`, "", 4)
	expectUpstream(t, "429 again", hs, path, hs.get(t, hs.claude, path), http.StatusTooManyRequests, `{"error":"rate_limited"}`, "", 5)

	hs.upstream.respond(path, usageTestResponse{drop: true})
	if dropped := hs.get(t, hs.claude, path); dropped.code != http.StatusBadGateway {
		t.Fatalf("transport error: status=%d body=%s, want 502", dropped.code, dropped.raw)
	}
}

// usageTestClaudeBody is a Claude /api/oauth/usage body (utilization in percent).
const usageTestClaudeBody = `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T23:00:00Z"},` +
	`"seven_day":{"utilization":40,"resets_at":"2026-10-08T21:00:00Z"}}`

// usageTestCodexBody is a Codex /wham/usage body (reset_at in unix seconds).
var usageTestCodexBody = fmt.Sprintf(`{"rate_limit":{`+
	`"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_at":%d},`+
	`"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_at":%d}}}`,
	usageTestStart.Add(2*time.Hour).Unix(), usageTestStart.Add(5*24*time.Hour).Unix())

// assertUsageReadings checks the windows recorded for authID by ID, used percent,
// kind and source, and that each was observed at observedAt.
func assertUsageReadings(t *testing.T, store *quotareading.Store, authID string, source quotareading.Source, observedAt time.Time, want map[string]float64) {
	t.Helper()
	reading := store.Get(authID)
	if len(reading.Windows) != len(want) {
		t.Fatalf("readings for %s = %+v, want windows %v", authID, reading.Windows, want)
	}
	for _, window := range reading.Windows {
		used, ok := want[window.ID]
		if !ok || window.UsedPercent != used || window.Source != source || !window.ObservedAt.Equal(observedAt) {
			t.Fatalf("readings for %s: window %+v, want used=%v source=%s observed=%s", authID, window, used, source, observedAt)
		}
	}
}

func TestUsageCacheRecordsReadingsOn2xx(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusInternalServerError, `{"five_hour":{"utilization":99}}`)
	hs.get(t, hs.claude, usageTestClaudeUsagePath)
	if windows := hs.readings.Get(hs.claude.ID).Windows; len(windows) != 0 {
		t.Fatalf("a 5xx body must not be recorded, got %+v", windows)
	}

	hs.clock.Advance(time.Minute)
	observed := hs.clock.Now()
	hs.upstream.respondJSON(usageTestClaudeUsagePath, http.StatusOK, usageTestClaudeBody)
	hs.get(t, hs.claude, usageTestClaudeUsagePath)
	assertUsageReadings(t, hs.readings, hs.claude.ID, quotareading.SourceUsage, observed, map[string]float64{"5h": 12.5, "7d": 40})
	if provider := hs.readings.Get(hs.claude.ID).Provider; provider != "claude" {
		t.Fatalf("provider = %q, want claude", provider)
	}

	hs.upstream.respondJSON(usageTestCodexUsagePath, http.StatusOK, usageTestCodexBody)
	hs.get(t, hs.codex, usageTestCodexUsagePath)
	assertUsageReadings(t, hs.readings, hs.codex.ID, quotareading.SourceUsage, observed, map[string]float64{"primary": 25, "secondary": 60})

	// Profile bodies carry no windows; a usage body under the profile URL is ignored.
	hs.clock.Advance(time.Minute)
	hs.upstream.respondJSON(usageTestClaudeProfilePath, http.StatusOK, `{"five_hour":{"utilization":99}}`)
	hs.get(t, hs.claude, usageTestClaudeProfilePath)
	assertUsageReadings(t, hs.readings, hs.claude.ID, quotareading.SourceUsage, observed, map[string]float64{"5h": 12.5, "7d": 40})
}

func TestUsageCacheKeyIgnoresCallerHeadersAndRoute(t *testing.T) {
	t.Parallel()
	hs := newUsageCacheHarness(t, config.RoutingConfig{})
	path := usageTestClaudeUsagePath
	hs.upstream.respondJSON(path, http.StatusOK, `{"n":1}`)
	expectUpstream(t, "v0", hs, path, hs.get(t, hs.claude, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 1)

	v8 := hs.mustDo(t, usageTestCall{
		route:     "/v8/management/requests/api-call",
		authIndex: " " + hs.claude.Index + " ",
		method:    "get",
		url:       hs.url(path),
		header:    map[string]string{"Authorization": "Bearer caller-supplied", "anthropic-beta": "oauth-2025-04-20"},
	})
	expectUpstream(t, "v8 with other headers", hs, path, v8, http.StatusOK, `{"n":1}`, usageCacheHit, 1)

	// Another credential has its own entry.
	other := hs.register(t, usageTestClaudeAuth("claude-b.json", "claude-b-token"))
	expectUpstream(t, "other credential", hs, path, hs.get(t, other, path), http.StatusOK, `{"n":1}`, usageCacheMiss, 2)
	if headers := hs.upstream.requestHeaders(path); len(headers) != 2 || headers[1].Get("Authorization") != "Bearer claude-b-token" {
		t.Fatalf("upstream headers = %v, want the second call with claude-b's token", headers)
	}
}

// usageTestRequest is a cache-level request for the leader/follower tests.
func usageTestRequest() usageRequest {
	return usageRequest{
		key:      usageCacheKey("idx", quotareading.ClaudeUsageURL),
		target:   usageTarget{endpoint: usageEndpointClaudeUsage, canonicalURL: quotareading.ClaudeUsageURL},
		authID:   "claude-a.json",
		provider: "claude",
		source:   quotareading.SourceUsage,
	}
}

// startFollower begins a second call for the same key while leader is in
// flight and returns a channel that yields its outcome once it stops waiting.
func startFollower(t *testing.T, cache *usageCache, ctx context.Context) <-chan *usageCacheCall {
	t.Helper()
	waiting := make(chan struct{})
	cache.testHookFollowerWaiting = func() { close(waiting) }
	outcome := make(chan *usageCacheCall, 1)
	go func() { outcome <- cache.begin(ctx, usageTestRequest(), config.UsageCacheConfig{}) }()
	usageTestReceive(t, waiting, "the follower to wait")
	return outcome
}

func TestUsageCacheLeaderExitsReleaseFollowers(t *testing.T) {
	t.Parallel()
	ok := apiCallResponse{StatusCode: http.StatusOK, Body: `{"n":1}`}
	failed := apiCallResponse{StatusCode: http.StatusBadGateway, Body: `{"error":"upstream"}`}
	cases := []struct {
		name string
		end  func(t *testing.T, leader *usageCacheCall)
		// want is the shared response, or nil when the follower must call
		// upstream itself.
		want *apiCallResponse
	}{
		{name: "early return", end: func(_ *testing.T, leader *usageCacheCall) { leader.release() }},
		{name: "transport error without cache", end: func(t *testing.T, leader *usageCacheCall) {
			if stale := leader.fail(); stale != nil {
				t.Errorf("fail() = %+v, want nil without a cached response", stale)
			}
			leader.release()
		}},
		{name: "2xx", want: &ok, end: func(_ *testing.T, leader *usageCacheCall) { leader.complete(ok); leader.release() }},
		{name: "5xx without cache", want: &failed, end: func(_ *testing.T, leader *usageCacheCall) { leader.complete(failed); leader.release() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := &usageCache{nowFunc: newUsageTestClock().Now, readings: quotareading.NewStore()}
			leader := cache.begin(context.Background(), usageTestRequest(), config.UsageCacheConfig{})
			if leader == nil || leader.served != nil || leader.flight == nil {
				t.Fatalf("leader = %+v, want an upstream call leading the key", leader)
			}
			outcome := startFollower(t, cache, context.Background())
			tc.end(t, leader)
			follower := usageTestReceive(t, outcome, "the follower to stop waiting")
			if follower == nil {
				t.Fatal("follower = nil, want a call")
			}
			got, served := follower.cachedResponse()
			switch {
			case tc.want == nil && served:
				t.Fatalf("follower served %+v, want it to call upstream itself", got)
			case tc.want == nil:
				if follower.flight == nil || follower.label != usageCacheMiss {
					t.Fatalf("follower = %+v, want the new leader of the key", follower)
				}
				follower.release()
			case !served || got.StatusCode != tc.want.StatusCode || got.Body != tc.want.Body || usageCacheLabel(got) != usageCacheMiss:
				t.Fatalf("follower served=%v %+v, want %+v labeled miss", served, got, *tc.want)
			}
			cache.mu.Lock()
			inFlight := cache.entries[usageTestRequest().key].flight
			cache.mu.Unlock()
			if inFlight != nil {
				t.Fatal("a finished key must not keep a flight")
			}
		})
	}
}

func TestUsageCacheCanceledFollowerStopsWaiting(t *testing.T) {
	t.Parallel()
	cache := &usageCache{nowFunc: newUsageTestClock().Now, readings: quotareading.NewStore()}
	leader := cache.begin(context.Background(), usageTestRequest(), config.UsageCacheConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	outcome := startFollower(t, cache, ctx)
	cancel()
	if follower := usageTestReceive(t, outcome, "the canceled follower to return"); follower != nil {
		t.Fatalf("canceled follower = %+v, want nil (pass-through)", follower)
	}
	// The leader is unaffected and still stores its response.
	leader.complete(apiCallResponse{StatusCode: http.StatusOK, Body: `{"n":1}`})
	leader.release()
	hit := cache.begin(context.Background(), usageTestRequest(), config.UsageCacheConfig{})
	if got, ok := hit.cachedResponse(); !ok || got.Body != `{"n":1}` || usageCacheLabel(got) != usageCacheHit {
		t.Fatalf("after leader: served=%v %+v, want a hit", ok, got)
	}
}

func TestClaudeUsageBackoffDoublesToCap(t *testing.T) {
	t.Parallel()
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 60 * time.Minute, 60 * time.Minute}
	for i, backoff := range want {
		if got := claudeUsageBackoff(i + 1); got != backoff {
			t.Fatalf("level %d: backoff = %s, want %s", i+1, got, backoff)
		}
	}
	if got := claudeUsageBackoff(1000); got != claudeUsageBackoffMax {
		t.Fatalf("level 1000: backoff = %s, want %s", got, claudeUsageBackoffMax)
	}
}

func TestUsageCacheOverlapping429DoesNotEscalateBackoff(t *testing.T) {
	t.Parallel()
	clock := newUsageTestClock()
	cache := &usageCache{nowFunc: clock.Now, readings: quotareading.NewStore()}
	req := usageTestRequest()
	cfg := config.UsageCacheConfig{}
	// Two followers wake without a result when the leader exits early. Both go
	// upstream: one leads the key again, the other calls on its own.
	leader := cache.begin(context.Background(), req, cfg)
	var waiting sync.WaitGroup
	waiting.Add(2)
	cache.testHookFollowerWaiting = waiting.Done
	outcomes := make(chan *usageCacheCall, 2)
	for range 2 {
		go func() { outcomes <- cache.begin(context.Background(), req, cfg) }()
	}
	usageTestWaitGroup(t, &waiting, "two followers to wait")
	leader.release()
	first := usageTestReceive(t, outcomes, "the first follower")
	second := usageTestReceive(t, outcomes, "the second follower")
	if first == nil || second == nil || first.served != nil || second.served != nil || (first.flight == nil) == (second.flight == nil) {
		t.Fatalf("calls = %+v, %+v, want one new leader and one independent upstream call", first, second)
	}
	// Both answer 429; only the first may start a backoff level.
	rejected := apiCallResponse{StatusCode: http.StatusTooManyRequests, Body: `{"error":"rate_limited"}`}
	first.complete(rejected)
	first.release()
	second.complete(rejected)
	second.release()
	cache.mu.Lock()
	entry := cache.entries[req.key]
	level, until := entry.backoffLevel, entry.backoffUntil
	cache.mu.Unlock()
	if level != 1 || !until.Equal(clock.Now().Add(5*time.Minute)) {
		t.Fatalf("backoff level=%d until=%s, want level 1 for 5m", level, until)
	}
}

func TestUsageCacheNilCallIsPassThrough(t *testing.T) {
	t.Parallel()
	var call *usageCacheCall
	if _, ok := call.cachedResponse(); ok {
		t.Fatal("nil call must not serve")
	}
	resp := apiCallResponse{StatusCode: http.StatusTeapot, Body: "x"}
	if got := call.complete(resp); got.StatusCode != resp.StatusCode || got.Body != resp.Body || got.Header != nil {
		t.Fatalf("complete = %+v, want the input unchanged", got)
	}
	if call.fail() != nil {
		t.Fatal("nil call must not answer from cache")
	}
	call.release()
}

func TestHandlerUsesDefaultReadingsStore(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	t.Cleanup(func() { devyreStates.Delete(h) })
	cache := h.devyreUsageCache()
	if cache != h.devyreUsageCache() {
		t.Fatal("a handler must keep one usage cache")
	}
	if cache.store() != quotareading.Default() {
		t.Fatal("the production cache must feed quotareading.Default()")
	}
	if len(cache.targets) != len(defaultUsageTargets()) {
		t.Fatalf("targets = %v, want the production allowlist", cache.targets)
	}
}
