package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

func TestShouldExposeResponsesUpstreamError_UnroutableModel(t *testing.T) {
	body := `{"error":{"message":"unknown provider for model gpt-x","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
	unroutable := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      fmt.Errorf("executing: %w", handlers.NewUnroutableModelError(body)),
	}
	if !shouldExposeResponsesUpstreamError(unroutable) {
		t.Fatal("no reconnect can make a model nobody serves routable: it must reach the client")
	}
	// An upstream credential's model_not_found stays silent, as upstream decided:
	// another credential may serve the model.
	upstream := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(body),
	}
	if shouldExposeResponsesUpstreamError(upstream) {
		t.Fatal("an upstream model_not_found must keep rotating credentials")
	}
}

// Over the real socket: a model nobody serves is answered with an error event at
// once, not a closed socket that Codex reconnects to five times.
func TestResponsesWebsocketAnswersAnUnroutableModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	request := `{"type":"response.create","model":"gpt-devyre-nobody-serves","input":[]}`
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("the socket closed without an answer: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeError {
		t.Fatalf("event type = %q, want %q: %s", got, wsEventTypeError, payload)
	}
	if got := gjson.GetBytes(payload, "status").Int(); got != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "error.code").String(); got != "model_not_found" {
		t.Fatalf("code = %q: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "error.message").String(); got != "unknown provider for model gpt-devyre-nobody-serves" {
		t.Fatalf("message = %q", got)
	}
}
