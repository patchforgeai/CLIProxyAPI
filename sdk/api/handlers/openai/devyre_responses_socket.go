package openai

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// devyreStartSocketReader starts the one reader of a Responses websocket client, in
// both modes. Without it, unless steering was on, the handler read the socket only
// between responses:
//   - a response.interrupt waited until the response it was meant to stop had run to
//     completion, and was then rejected with an "error" event that failed the client's
//     next request;
//   - a client that went away mid-response was noticed only when a later write failed,
//     and the upstream response kept running until then.
//
// The reader handles each response.interrupt at once (devyreInterruptResponse) and
// queues every other frame for the handler loop, or for a duplex stream when steering
// is on. A failed read, such as the client closing its socket, cancels the request's
// context with the read error as the cause, which ends the response being forwarded
// and, when the handler returns, its upstream socket.
func (h *OpenAIResponsesAPIHandler) devyreStartSocketReader(c *gin.Context, conn *websocket.Conn, writer *responsesWebsocketWriter, sessionID string) (<-chan cliproxyexecutor.WebsocketInput, func()) {
	socketCtx, cancelSocket := context.WithCancelCause(c.Request.Context())
	c.Request = c.Request.WithContext(socketCtx)
	interruptCtx := cliproxyexecutor.WithWebsocketAuthCheck(socketCtx, h.devyreSessionAuthEnabled(sessionID))
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
			if cliproxyexecutor.DevyreIsResponseInterrupt(payload) {
				if errInterrupt := h.devyreInterruptResponse(interruptCtx, writer, sessionID, payload); errInterrupt != nil {
					cancelSocket(errInterrupt)
					return
				}
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
