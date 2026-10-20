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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyre: /v8/management/routing/focus sits behind management auth and is not
// added to the deprecated v0 API.
func TestManagementV8RoutingFocusRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "port: 8317\nremote-management: {secret-key: test-password}\nrouting: {strategy: expiring-first}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = filepath.Dir(path)
	manager := coreauth.NewManager(nil, nil, nil)
	const token = "sk-ant-oat01-focus-route-secret"
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "claude-focus-route.json",
		FileName:   "claude-focus-route.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"access_token": token},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := management.NewHandler(cfg, path, manager)
	h.SetLocalPassword("test-password")
	store := routingfocus.NewStore()
	h.SetRoutingFocusStore(store)
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()

	request := func(method, url, body string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		recorder := httptest.NewRecorder()
		s.engine.ServeHTTP(recorder, req)
		return recorder
	}
	const put = `{"name":"claude-focus-route.json","until":null}`

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		body := ""
		if method == http.MethodPut {
			body = put
		}
		if got := request(method, "/v8/management/routing/focus?provider=claude", body, false); got.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a key: status=%d body=%s", method, got.Code, got.Body.String())
		}
		if got := request(method, "/v0/management/routing/focus?provider=claude", body, true); got.Code != http.StatusNotFound {
			t.Fatalf("%s on v0: status=%d, want 404", method, got.Code)
		}
	}
	if !store.Empty() {
		t.Fatal("an unauthorized request changed the focus")
	}

	decode := func(got *httptest.ResponseRecorder) []string {
		t.Helper()
		if got.Code != http.StatusOK || strings.Contains(got.Body.String(), token) {
			t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
		}
		var body struct {
			Focus []struct {
				AuthID string `json:"auth_id"`
			} `json:"focus"`
		}
		if errDecode := json.Unmarshal(got.Body.Bytes(), &body); errDecode != nil {
			t.Fatal(errDecode)
		}
		ids := make([]string, 0, len(body.Focus))
		for _, focus := range body.Focus {
			ids = append(ids, focus.AuthID)
		}
		return ids
	}
	if ids := decode(request(http.MethodPut, "/v8/management/routing/focus", put, true)); len(ids) != 1 || ids[0] != "claude-focus-route.json" {
		t.Fatalf("PUT focus = %v", ids)
	}
	if ids := decode(request(http.MethodGet, "/v8/management/routing/focus", "", true)); len(ids) != 1 || !store.Focused("claude-focus-route.json", time.Now()) {
		t.Fatalf("GET focus = %v", ids)
	}
	// The readings report it too.
	readings := request(http.MethodGet, "/v8/management/routing/quota-readings", "", true)
	if readings.Code != http.StatusOK || !strings.Contains(readings.Body.String(), `"rank":1,"focused":true,"focus_until":null`) {
		t.Fatalf("readings = %d %s", readings.Code, readings.Body.String())
	}
	if ids := decode(request(http.MethodDelete, "/v8/management/routing/focus?name=claude-focus-route.json", "", true)); len(ids) != 0 || !store.Empty() {
		t.Fatalf("DELETE focus = %v", ids)
	}
}
