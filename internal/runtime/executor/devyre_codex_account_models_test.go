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

func devyreCodexModelsAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "codex-account.json",
		Provider:   "codex",
		Attributes: map[string]string{"base_url": baseURL},
		Metadata: map[string]any{
			"access_token": "access-token",
			"account_id":   "account-123",
		},
	}
}

func TestDevyreFetchAccountModels_AsksAsCodexDoes(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6.1-terra"}]}`))
	}))
	defer server.Close()

	auto := NewCodexAutoExecutor(&config.Config{})
	body, err := auto.DevyreFetchAccountModels(context.Background(), devyreCodexModelsAuth(server.URL+"/"), "0.163.0")
	if err != nil {
		t.Fatalf("DevyreFetchAccountModels: %v", err)
	}
	if string(body) != `{"models":[{"slug":"gpt-6.1-terra"}]}` {
		t.Fatalf("body = %s", body)
	}
	if got == nil {
		t.Fatal("no request reached the catalog")
	}
	if got.Method != http.MethodGet || got.URL.Path != "/models" || got.URL.Query().Get("client_version") != "0.163.0" {
		t.Fatalf("request = %s %s", got.Method, got.URL)
	}
	for header, want := range map[string]string{
		"Authorization":      "Bearer access-token",
		"Chatgpt-Account-Id": "account-123",
		"Originator":         codexOriginator,
		"User-Agent":         codexUserAgent,
		"Accept":             "application/json",
	} {
		if value := got.Header.Get(header); value != want {
			t.Fatalf("%s = %q, want %q", header, value, want)
		}
	}
	if got.Header.Get("Content-Type") != "" {
		t.Fatal("a GET carries no content type")
	}
}

func TestDevyreFetchAccountModels_Refusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"account secret-detail"}`))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{})
	_, err := exec.DevyreFetchAccountModels(context.Background(), devyreCodexModelsAuth(server.URL), "0.163.0")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the status", err)
	}
	if strings.Contains(err.Error(), "secret-detail") {
		t.Fatalf("the body is never echoed: %v", err)
	}

	noToken := devyreCodexModelsAuth(server.URL)
	noToken.Metadata = map[string]any{}
	if _, err := exec.DevyreFetchAccountModels(context.Background(), noToken, "0.163.0"); err == nil {
		t.Fatal("a credential without a token is not asked")
	}
	if _, err := exec.DevyreFetchAccountModels(context.Background(), nil, "0.163.0"); err == nil {
		t.Fatal("no credential, no lookup")
	}
	var auto *CodexAutoExecutor
	if _, err := auto.DevyreFetchAccountModels(context.Background(), noToken, "0.163.0"); err == nil {
		t.Fatal("a nil auto executor refuses")
	}
}
