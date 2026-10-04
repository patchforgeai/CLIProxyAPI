package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyre: GET /v8/management/routing/quota-readings sits behind management auth
// and is not added to the deprecated v0 API.
func TestManagementV8QuotaReadingsRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "port: 8317\nremote-management: {secret-key: test-password}\nrouting: {strategy: soonest-reset}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = filepath.Dir(path)
	manager := coreauth.NewManager(nil, nil, nil)
	const token = "sk-ant-oat01-route-test-secret"
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "claude-quota-readings-route.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"access_token": token},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := management.NewHandler(cfg, path, manager)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()

	request := func(url string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		recorder := httptest.NewRecorder()
		s.engine.ServeHTTP(recorder, req)
		return recorder
	}

	if got := request("/v8/management/routing/quota-readings", false); got.Code != http.StatusUnauthorized {
		t.Fatalf("without a key: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := request("/v0/management/routing/quota-readings", true); got.Code != http.StatusNotFound {
		t.Fatalf("v0 route: status=%d, want 404", got.Code)
	}
	got := request("/v8/management/routing/quota-readings", true)
	if got.Code != http.StatusOK || strings.Contains(got.Body.String(), token) {
		t.Fatalf("with the key: status=%d body=%s", got.Code, got.Body.String())
	}
	var body struct {
		Strategy    string `json:"strategy"`
		Credentials []struct {
			AuthID string `json:"auth_id"`
			Rank   int    `json:"rank"`
		} `json:"credentials"`
	}
	if err = json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Strategy != "expiring-first" || len(body.Credentials) != 1 ||
		body.Credentials[0].AuthID != "claude-quota-readings-route.json" || body.Credentials[0].Rank != 1 {
		t.Fatalf("body = %s", got.Body.String())
	}
}
