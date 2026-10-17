package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func keepAliveStatus(t *testing.T, s *Server, secret string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/keep-alive", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	recorder := httptest.NewRecorder()
	s.engine.ServeHTTP(recorder, request)
	return recorder.Code
}

// devyre: the lifeline's /keep-alive answers only the local password and starts no idle watcher.
func TestDevyreKeepAliveProbe(t *testing.T) {
	s := newTestServerWithOptions(t, WithLocalManagementPassword("probe-password"), WithDevyreKeepAliveProbe())
	if got := keepAliveStatus(t, s, "probe-password"); got != http.StatusOK {
		t.Fatalf("with the password: status %d, want 200", got)
	}
	if got := keepAliveStatus(t, s, "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("with a wrong password: status %d, want 401", got)
	}
	if got := keepAliveStatus(t, s, ""); got != http.StatusUnauthorized {
		t.Fatalf("without a password: status %d, want 401", got)
	}
	if s.keepAliveEnabled || s.keepAliveStop != nil {
		t.Fatal("the probe started the idle watcher")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

// devyre: next to WithKeepAliveEndpoint the probe adds nothing (gin panics on a second route)
// and the idle limit stays.
func TestDevyreKeepAliveProbeWithTheIdleLimit(t *testing.T) {
	s := newTestServerWithOptions(t,
		WithLocalManagementPassword("probe-password"),
		WithKeepAliveEndpoint(time.Hour, func() {}),
		WithDevyreKeepAliveProbe(),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() {
		if err := s.Stop(ctx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()
	if !s.keepAliveEnabled {
		t.Fatal("the idle limit is gone")
	}
	if got := keepAliveStatus(t, s, "probe-password"); got != http.StatusOK {
		t.Fatalf("with the password: status %d, want 200", got)
	}
}

func TestWithoutDevyreKeepAliveProbeThereIsNoKeepAlive(t *testing.T) {
	s := newTestServerWithOptions(t, WithLocalManagementPassword("probe-password"))
	if got := keepAliveStatus(t, s, "probe-password"); got != http.StatusNotFound {
		t.Fatalf("status %d, want 404", got)
	}
}
