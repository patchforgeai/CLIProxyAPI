package openai

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// devyreStartSocketReader starts the one reader of a Responses websocket client, in
// both modes. Without it, unless steering was on, the handler read the socket only
// between responses, so a client that went away mid-response was noticed only when a
// later write failed, and the upstream response kept running until then.
//
// The reader queues every frame for the handler loop, or for a duplex stream when
// steering is on. A failed read, such as the client closing its socket, cancels the
// request's context with the read error as the cause, which ends the response being
// forwarded and, when the handler returns, its upstream socket.
func (h *OpenAIResponsesAPIHandler) devyreStartSocketReader(c *gin.Context, conn *websocket.Conn) (<-chan cliproxyexecutor.WebsocketInput, func()) {
	socketCtx, cancelSocket := context.WithCancelCause(c.Request.Context())
	c.Request = c.Request.WithContext(socketCtx)
	// The bounded queue backpressures clients instead of retaining unlimited input.
	input := make(chan cliproxyexecutor.WebsocketInput, 16)
	go func() {
		defer close(input)
		for {
			kind, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				cancelSocket(errRead)
				return
			}
			if kind != websocket.TextMessage && kind != websocket.BinaryMessage {
				continue
			}
			select {
			case input <- cliproxyexecutor.WebsocketInput{Payload: payload}:
			case <-socketCtx.Done():
				return
			}
		}
	}()
	return input, func() { cancelSocket(context.Canceled) }
}
