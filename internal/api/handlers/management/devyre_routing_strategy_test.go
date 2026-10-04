package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNormalizeRoutingStrategyExpiringFirstAliases(t *testing.T) {
	for _, input := range []string{"expiring-first", "expiringfirst", "ef", "soonest-reset", " Expiring-First ", "EF"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "expiring-first" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want expiring-first, true", input, got, ok)
		}
	}
	if got, ok := normalizeRoutingStrategy("expiring-last"); ok {
		t.Fatalf("normalizeRoutingStrategy(expiring-last) = %q, true; want rejection", got)
	}
}

func TestPutRoutingStrategyAcceptsExpiringFirstAlias(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("routing:\n  strategy: round-robin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	router := gin.New()
	router.PUT("/routing/strategy", h.PutRoutingStrategy)
	router.GET("/routing/strategy", h.GetRoutingStrategy)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/routing/strategy", strings.NewReader(`{"value":"soonest-reset"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/routing/strategy", nil))
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"strategy":"expiring-first"}` {
		t.Fatalf("GET body = %s", got)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil || loaded.Routing.Strategy != "expiring-first" {
		t.Fatalf("saved strategy = %q, %v; want expiring-first", loaded.Routing.Strategy, err)
	}
}
