package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func devyreListCredentials(t *testing.T, h *Handler, v8 bool) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	path := "/v0/management/auth-files"
	if v8 {
		path = "/v8/management/credentials"
		ctx.Set(ConfigV8ContextKey, true)
	}
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	h.ListAuthFiles(ctx)
	var listing struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil || len(listing.Files) != 1 {
		t.Fatalf("listing: %v %s", err, rec.Body.String())
	}
	return listing.Files[0]
}

// devyre: the v8 credential listing says when each sign-in ends; the deprecated v0 one is
// left as upstream has it.
func TestDevyreTheV8ListingSaysWhenTheSignInEnds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "claude-a@example.com.json",
		FileName:   "claude-a@example.com.json",
		Provider:   "claude",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": "a@example.com", coreauth.MetadataSignInEndsAt: "2026-12-20T10:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	entry := devyreListCredentials(t, h, true)
	signIn, _ := entry["sign_in"].(map[string]any)
	if signIn["ends_at"] != "2026-12-20T10:00:00Z" {
		t.Fatalf("v8 sign_in = %#v", entry["sign_in"])
	}
	if entry := devyreListCredentials(t, h, false); entry["sign_in"] != nil {
		t.Fatalf("v0 lists sign_in: %#v", entry["sign_in"])
	}
}

func TestDevyreTheV8ListingSaysHowTheSignInStands(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "claude-a@example.com.json",
		FileName:   "claude-a@example.com.json",
		Provider:   "claude",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata: map[string]any{
			"email":        "a@example.com",
			"access_token": "not-a-real-token",
			"expired":      "2026-11-26T10:00:00Z",
			coreauth.MetadataSignInEnded: map[string]any{
				"reason": "ended", "answer": "invalid_grant: Refresh token expired", "at": "2026-11-26T06:05:04Z",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	signIn, _ := devyreListCredentials(t, h, true)["sign_in"].(map[string]any)
	if signIn["state"] != "ended" || signIn["reason"] != "ended" || signIn["answer"] != "invalid_grant: Refresh token expired" ||
		signIn["since"] != "2026-11-26T06:05:04Z" || signIn["access_expires_at"] != "2026-11-26T10:00:00Z" {
		t.Fatalf("v8 sign_in = %#v", signIn)
	}
	if _, retried := signIn["next_try_at"]; retried {
		t.Fatalf("an ended sign-in lists a next try: %#v", signIn)
	}
	if signIn["renews"] != false {
		t.Fatalf("an ended sign-in is listed as renewed: %#v", signIn)
	}
}

func TestDevyreTheV8ListingSaysWhetherTheRouterRenewsTheToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "claude-b@example.com.json", FileName: "claude-b@example.com.json", Provider: "claude",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata: map[string]any{
			"email": "b@example.com", "access_token": "not-a-real-token", "refresh_token": "not-a-real-refresh-token",
			"expired": "2026-11-26T10:00:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	signIn, _ := devyreListCredentials(t, h, true)["sign_in"].(map[string]any)
	if signIn["renews"] != true || signIn["state"] != nil {
		t.Fatalf("v8 sign_in = %#v, want renewed and fine", signIn)
	}
}

func TestDevyreTheV8ListingListsASignInWithNothingToSay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "claude-c@example.com.json", FileName: "claude-c@example.com.json", Provider: "claude",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": "c@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	if signIn, ok := devyreListCredentials(t, h, true)["sign_in"].(map[string]any); !ok || len(signIn) != 0 {
		t.Fatalf("v8 sign_in = %#v, want an empty object", signIn)
	}
}
