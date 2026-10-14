package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func devyreOAuthStart(t *testing.T, router http.Handler, query string) (int, string, string) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?"+query, nil))
	var payload struct {
		URL   string `json:"url"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(w.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), errDecode)
	}
	if payload.State != "" {
		t.Cleanup(func() { CompleteOAuthSession(payload.State) })
	}
	if w.Code != http.StatusOK {
		return w.Code, payload.Error, ""
	}
	authURL, errParse := url.Parse(payload.URL)
	if errParse != nil {
		t.Fatalf("parse %q: %v", payload.URL, errParse)
	}
	return w.Code, authURL.Query().Get("redirect_uri"), payload.State
}

func devyreOAuthRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authDir := filepath.Join(t.TempDir(), "auths")
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", handler.StartOAuthV8)
	return router, authDir
}

func TestDevyreRedirectPortClaude(t *testing.T) {
	router, _ := devyreOAuthRouter(t)

	status, redirect, _ := devyreOAuthStart(t, router, "provider=claude")
	if status != http.StatusOK || redirect != "http://localhost:54545/callback" {
		t.Fatalf("without redirect_port: %d %q, want upstream's fixed redirect", status, redirect)
	}
	status, redirect, _ = devyreOAuthStart(t, router, "provider=claude&redirect_port=51234")
	if status != http.StatusOK || redirect != "http://localhost:51234/callback" {
		t.Fatalf("with redirect_port: %d %q", status, redirect)
	}
}

func TestDevyreRedirectPortRefusesBadValues(t *testing.T) {
	router, _ := devyreOAuthRouter(t)
	for _, provider := range []string{"claude", "codex"} {
		for _, value := range []string{"", "0", "65536", "-1", "+80", "80x", "1e3", "%2051234", "123456", "0x50"} {
			status, message, _ := devyreOAuthStart(t, router, "provider="+provider+"&redirect_port="+value)
			if status != http.StatusBadRequest || message != "invalid redirect_port" {
				t.Fatalf("%s redirect_port=%q: %d %q, want 400 invalid redirect_port", provider, value, status, message)
			}
		}
		status, message, _ := devyreOAuthStart(t, router, "provider="+provider+"&redirect_port=51234&is_webui=true")
		if status != http.StatusBadRequest || message != "redirect_port cannot be combined with is_webui" {
			t.Fatalf("%s with is_webui: %d %q", provider, status, message)
		}
	}
	// The bounds themselves are ports.
	for _, value := range []string{"1", "65535"} {
		if status, _, _ := devyreOAuthStart(t, router, "provider=claude&redirect_port="+value); status != http.StatusOK {
			t.Fatalf("redirect_port=%s: %d", value, status)
		}
	}
}

func TestDevyreRedirectPortCodexAuthorizeURL(t *testing.T) {
	router, _ := devyreOAuthRouter(t)
	status, redirect, _ := devyreOAuthStart(t, router, "provider=codex")
	if status != http.StatusOK || redirect != "http://localhost:1455/auth/callback" {
		t.Fatalf("without redirect_port: %d %q, want upstream's fixed redirect", status, redirect)
	}
	status, redirect, _ = devyreOAuthStart(t, router, "provider=codex&redirect_port=51234")
	if status != http.StatusOK || redirect != "http://127.0.0.1:51234/auth/callback" {
		t.Fatalf("with redirect_port: %d %q", status, redirect)
	}
}

// devyreRecordingCodexService is the concurrency test's fake that also records which redirect
// each call used.
type devyreRecordingCodexService struct {
	fakeCodexOAuthService
	mu        sync.Mutex
	exchanges []string
}

func (f *devyreRecordingCodexService) GenerateAuthURLWithRedirect(state string, pkceCodes *codex.PKCECodes, redirectURI string) (string, error) {
	return "https://auth.example.test/oauth?" + url.Values{"state": {state}, "redirect_uri": {redirectURI}}.Encode(), nil
}

func (f *devyreRecordingCodexService) ExchangeCodeForTokens(ctx context.Context, code string, pkceCodes *codex.PKCECodes) (*codex.CodexAuthBundle, error) {
	f.record(codex.RedirectURI)
	return f.fakeCodexOAuthService.ExchangeCodeForTokens(ctx, code, pkceCodes)
}

func (f *devyreRecordingCodexService) ExchangeCodeForTokensWithRedirect(ctx context.Context, code, redirectURI string, pkceCodes *codex.PKCECodes) (*codex.CodexAuthBundle, error) {
	f.record(redirectURI)
	return f.fakeCodexOAuthService.ExchangeCodeForTokens(ctx, code, pkceCodes)
}

func (f *devyreRecordingCodexService) record(redirectURI string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges = append(f.exchanges, redirectURI)
}

func (f *devyreRecordingCodexService) exchanged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.exchanges...)
}

func TestDevyreRedirectPortCodexExchangeUsesTheSameRedirect(t *testing.T) {
	service := &devyreRecordingCodexService{}
	original := newCodexOAuthService
	newCodexOAuthService = func(cfg *config.Config) codexOAuthService { return service }
	defer func() { newCodexOAuthService = original }()
	router, authDir := devyreOAuthRouter(t)

	status, redirect, state := devyreOAuthStart(t, router, "provider=codex&redirect_port=51234")
	if status != http.StatusOK || redirect != "http://127.0.0.1:51234/auth/callback" {
		t.Fatalf("start: %d %q", status, redirect)
	}
	if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, "codex", state, "moved-code", ""); errWrite != nil {
		t.Fatal(errWrite)
	}
	waitForOAuthSessionDone(t, state)

	status, redirect, state = devyreOAuthStart(t, router, "provider=codex")
	if status != http.StatusOK || redirect != "" {
		// The fake's own URL carries no redirect; the default path never asks for one.
		t.Fatalf("default start: %d %q", status, redirect)
	}
	if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, "codex", state, "fixed-code", ""); errWrite != nil {
		t.Fatal(errWrite)
	}
	waitForOAuthSessionDone(t, state)

	got := service.exchanged()
	want := []string{"http://127.0.0.1:51234/auth/callback", codex.RedirectURI}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("exchange redirects = %v, want %v", got, want)
	}
}

func TestDevyreRedirectPortCodexServiceWithoutRedirects(t *testing.T) {
	original := newCodexOAuthService
	newCodexOAuthService = func(cfg *config.Config) codexOAuthService { return &fakeCodexOAuthService{} }
	defer func() { newCodexOAuthService = original }()
	router, _ := devyreOAuthRouter(t)

	if status, _, _ := devyreOAuthStart(t, router, "provider=codex&redirect_port=51234"); status != http.StatusInternalServerError {
		t.Fatalf("a service that cannot move the redirect answered %d, want 500", status)
	}
	if status, _, _ := devyreOAuthStart(t, router, "provider=codex"); status != http.StatusOK {
		t.Fatalf("the default start answered %d", status)
	}
}
