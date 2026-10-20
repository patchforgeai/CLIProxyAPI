package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// devyreSocketHarness runs the real handler, auth manager and Codex executor against
// a scripted upstream websocket. Its timeouts only bound a failing test: every step waits
// for a protocol event, never for time to pass.
type devyreSocketHarness struct {
	t           *testing.T
	client      *websocket.Conn
	manager     *coreauth.Manager
	auth        *coreauth.Auth
	model       string
	upstreamEnd chan struct{}
	connections atomic.Int32
	frames      atomic.Int32
}

type devyreUpstreamConn struct {
	t       *testing.T
	conn    *websocket.Conn
	counter *atomic.Int32
}

func (u devyreUpstreamConn) read() []byte {
	u.t.Helper()
	_, b, err := u.conn.ReadMessage()
	if err != nil {
		u.t.Errorf("upstream read: %v", err)
		return nil
	}
	u.counter.Add(1)
	return b
}

func (u devyreUpstreamConn) write(s string) {
	u.t.Helper()
	if err := u.conn.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
		u.t.Errorf("upstream write: %v", err)
	}
}

// waitClosed returns once the client side of the upstream socket is gone.
func (u devyreUpstreamConn) waitClosed() {
	for {
		if _, _, err := u.conn.ReadMessage(); err != nil {
			return
		}
		u.counter.Add(1)
	}
}

func newDevyreSocketHarness(t *testing.T, steering bool, name string, script func(u devyreUpstreamConn)) *devyreSocketHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &devyreSocketHarness{t: t, upstreamEnd: make(chan struct{}), model: "devyre-socket-" + name}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.connections.Add(1) > 1 {
			t.Error("unexpected second upstream connection")
			return
		}
		defer close(h.upstreamEnd)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		script(devyreUpstreamConn{t: t, conn: conn, counter: &h.frames})
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{}
	cfg.Codex.ResponseSteering = steering
	cfg.CodexResponseSteering = steering
	h.manager = coreauth.NewManager(nil, nil, nil)
	h.manager.SetConfig(cfg)
	h.manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	authID := "devyre-socket-" + name
	auth, err := h.manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	h.auth = auth
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: h.model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, h.manager))
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	h.client = client
	return h
}

func (h *devyreSocketHarness) send(payload string) {
	h.t.Helper()
	if err := h.client.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		h.t.Fatal(err)
	}
}

// expect reads the next client frame and checks its type and, when given, its response id.
func (h *devyreSocketHarness) expect(eventType, responseID string) []byte {
	h.t.Helper()
	_, payload, err := h.client.ReadMessage()
	if err != nil {
		h.t.Fatalf("waiting for %s: %v", eventType, err)
	}
	if gjson.GetBytes(payload, "type").String() != eventType {
		h.t.Fatalf("expected %s, got %s", eventType, payload)
	}
	if responseID != "" && gjson.GetBytes(payload, "response.id").String() != responseID && gjson.GetBytes(payload, "response_id").String() != responseID {
		h.t.Fatalf("expected %s for %s, got %s", eventType, responseID, payload)
	}
	return payload
}

func (h *devyreSocketHarness) waitUpstreamClosed() {
	h.t.Helper()
	select {
	case <-h.upstreamEnd:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the upstream socket stayed open")
	}
}

func devyreBothModes(t *testing.T, run func(t *testing.T, steering bool)) {
	for _, steering := range []bool{false, true} {
		name := "per-response"
		if steering {
			name = "duplex"
		}
		t.Run(name, func(t *testing.T) { run(t, steering) })
	}
}

func devyreModeName(steering bool) string {
	if steering {
		return "duplex"
	}
	return "per-response"
}

// Codex closes its socket when the user stops a turn. The router must close the upstream
// socket then, not when the abandoned response would have finished: here the upstream
// sends nothing after response.created, so only reading the client socket can notice.
func TestDevyreResponsesClientDisconnectClosesTheUpstreamSocket(t *testing.T) {
	devyreBothModes(t, func(t *testing.T, steering bool) {
		h := newDevyreSocketHarness(t, steering, "disconnect-"+devyreModeName(steering), func(u devyreUpstreamConn) {
			u.read()
			u.write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
			u.waitClosed()
		})
		h.send(`{"type":"response.create","model":"` + h.model + `","input":[]}`)
		h.expect("response.created", "r1")
		if err := h.client.Close(); err != nil {
			t.Fatal(err)
		}
		h.waitUpstreamClosed()
	})
}
