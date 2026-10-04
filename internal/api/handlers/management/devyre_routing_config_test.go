package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The expiring-first and quota-observation blocks must survive the real
// /v8/management/config write path and a later v0 save of an unrelated field.
func TestExpiringFirstRoutingConfigSurvivesV8ManagementAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# Operator comment\nconfig-version: 8\nserver:\n  port: 8317\nrouting:\n  strategy: round-robin\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	v8 := router.Group("/v8/management", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	v8.PATCH("/config", h.ConfigV8)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		v8.Handle(method, "/config/*path", h.ConfigV8)
	}
	router.PUT("/v0/management/routing/strategy", h.PutRoutingStrategy)
	request := func(method, url, body string, status int) string {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, url, strings.NewReader(body)))
		if recorder.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, url, recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}
	assertBlocks := func(stage string) *config.Config {
		t.Helper()
		loaded, errLoad := config.LoadConfig(path)
		if errLoad != nil {
			t.Fatalf("%s: %v", stage, errLoad)
		}
		routing := loaded.Routing
		cache, poller := routing.QuotaObservation.UsageCache, routing.QuotaObservation.Poller
		if routing.ExpiringFirst.GateRemainingPercent == nil || routing.ExpiringFirst.GatePercent() != 0 || !routing.ExpiringFirst.LogPicks ||
			cache.Enabled == nil || cache.IsEnabled() || cache.ClaudeUsageTTLDuration() != 7*time.Minute ||
			poller.Enabled == nil || routing.QuotaPollerEnabled() || poller.ClaudeMinGapDuration() != 15*time.Minute || poller.CodexIntervalDuration() != 6*time.Minute {
			t.Fatalf("%s: routing = %+v", stage, routing)
		}
		saved, _ := os.ReadFile(path)
		if errValidate := config.ValidateV8Config(saved); errValidate != nil {
			t.Fatalf("%s: saved config is not valid v8: %v", stage, errValidate)
		}
		if !strings.Contains(string(saved), "# Operator comment") || strings.Contains(string(saved), "# routing.") {
			t.Fatalf("%s: comments were lost or routing keys were archived:\n%s", stage, saved)
		}
		return loaded
	}

	request(http.MethodPatch, "/v8/management/config", `{"routing":{"strategy":"expiring-first",
		"expiring-first":{"gate-remaining-percent":0,"log-picks":true},
		"quota-observation":{"usage-cache":{"enabled":false,"claude-usage-ttl":"7m"},"poller":{"enabled":false,"claude-min-gap":"15m"}}}}`, http.StatusOK)
	request(http.MethodPut, "/v8/management/config/routing/quota-observation/poller/codex-interval", `"6m"`, http.StatusOK)
	if loaded := assertBlocks("v8 write"); !config.IsExpiringFirstStrategy(loaded.Routing.Strategy) {
		t.Fatalf("strategy = %q", loaded.Routing.Strategy)
	}

	var view map[string]any
	if err = json.Unmarshal([]byte(request(http.MethodGet, "/v8/management/config/routing/expiring-first", "", http.StatusOK)), &view); err != nil {
		t.Fatal(err)
	}
	if view["gate-remaining-percent"] != float64(0) || view["log-picks"] != true {
		t.Fatalf("GET expiring-first = %v", view)
	}

	request(http.MethodPut, "/v0/management/routing/strategy", `{"value":"fill-first"}`, http.StatusOK)
	if loaded := assertBlocks("v0 save"); loaded.Routing.Strategy != "fill-first" {
		t.Fatalf("strategy = %q, want fill-first", loaded.Routing.Strategy)
	}

	request(http.MethodPatch, "/v8/management/config", `{"routing":{"expiring-first":{"gate-percent":5}}}`, http.StatusBadRequest)
	request(http.MethodDelete, "/v8/management/config/routing/quota-observation/poller/enabled", "", http.StatusOK)
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.QuotaObservation.Poller.Enabled != nil || loaded.Routing.QuotaPollerEnabled() {
		t.Fatalf("deleting poller.enabled did not restore the strategy default: %+v", loaded.Routing.QuotaObservation.Poller)
	}
}
