package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestDevyreFetchAccountModels_ClaudeAsksWithTheAccountsToken(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-fable-5-2"}],"has_more":false}`))
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{
		ID:         "claude-account.json",
		Provider:   "claude",
		Attributes: map[string]string{"base_url": server.URL + "/", "auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "sk-ant-oat01-token"},
	}
	body, err := NewClaudeExecutor(&config.Config{}).DevyreFetchAccountModels(context.Background(), auth, "claude-opus-5-5")
	if err != nil {
		t.Fatalf("DevyreFetchAccountModels: %v", err)
	}
	if !strings.Contains(string(body), "claude-fable-5-2") {
		t.Fatalf("body = %s", body)
	}
	if got == nil {
		t.Fatal("no request reached the list")
	}
	if got.Method != http.MethodGet || got.URL.Path != "/v1/models" ||
		got.URL.Query().Get("limit") != "1000" || got.URL.Query().Get("after_id") != "claude-opus-5-5" {
		t.Fatalf("request = %s %s", got.Method, got.URL)
	}
	for header, want := range map[string]string{
		"Authorization":     "Bearer sk-ant-oat01-token",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    claudeOAuthBeta,
		"Accept":            "application/json",
	} {
		if value := got.Header.Get(header); value != want {
			t.Fatalf("%s = %q, want %q", header, value, want)
		}
	}
}

func TestDevyreFetchAccountModels_ClaudeRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"account details"}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "claude-account.json", Provider: "claude", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "token"}}
	_, err := executor.DevyreFetchAccountModels(context.Background(), auth, "")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "account details") {
		t.Fatalf("err = %v, want the status without the body", err)
	}
	if _, err := executor.DevyreFetchAccountModels(context.Background(), &cliproxyauth.Auth{ID: "empty", Provider: "claude"}, ""); err == nil {
		t.Fatal("a credential without a token asks nothing")
	}
}
