package executor

import (
	"context"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// devyreEndedResponsesKept bounds the ended responses a session remembers for its socket.
// Codex interrupts the response it is reading, so only the latest ones matter: one whose
// terminal event is still on its way to the client when the interrupt arrives.
const devyreEndedResponsesKept = 8

// devyreCodexResponses follows the responses on a session's upstream socket, as the
// session's upstream reader reads them, so that a client's response.interrupt is written
// to that socket only while the response it names runs there.
//
// Writing it at any other time does harm: upstream would answer an interrupt for a
// response that already ended, and between requests nothing reads that answer for the
// client, so it would reach the client as the first event of its next request, where an
// "error" event fails that request.
type devyreCodexResponses struct {
	mu   sync.Mutex
	conn *websocket.Conn
	// running is the response created on conn whose terminal event has not been read;
	// runningInterrupted says that this router wrote an interrupt for it.
	running            string
	runningInterrupted bool
	// ended holds the latest responses that ended on conn, oldest first.
	ended []devyreEndedResponse
}

type devyreEndedResponse struct {
	id          string
	interrupted bool
}

type devyreResponseState int

const (
	devyreResponseUnknown devyreResponseState = iota
	devyreResponseRunning
	devyreResponseEnded
)

// resetLocked starts following conn, a socket that replaced the previous one.
func (r *devyreCodexResponses) resetLocked(conn *websocket.Conn) {
	r.conn = conn
	r.running = ""
	r.runningInterrupted = false
	r.ended = nil
}

func (r *devyreCodexResponses) endRunningLocked() {
	if r.running == "" {
		return
	}
	r.rememberEndedLocked(r.running, r.runningInterrupted)
	r.running = ""
	r.runningInterrupted = false
}

func (r *devyreCodexResponses) rememberEndedLocked(id string, interrupted bool) {
	if id == "" {
		return
	}
	if len(r.ended) >= devyreEndedResponsesKept {
		r.ended = append(r.ended[:0], r.ended[1:]...)
	}
	r.ended = append(r.ended, devyreEndedResponse{id: id, interrupted: interrupted})
}

func (r *devyreCodexResponses) stateLocked(conn *websocket.Conn, id string) devyreResponseState {
	if r.conn != conn || id == "" {
		return devyreResponseUnknown
	}
	if r.running == id {
		return devyreResponseRunning
	}
	for _, ended := range r.ended {
		if ended.id == id {
			return devyreResponseEnded
		}
	}
	return devyreResponseUnknown
}

// devyreObserveUpstream records an event read from conn. The session's upstream reader
// calls it for every text frame, before the frame reaches the request that reads it, so
// the state is never behind what any client was sent.
func (s *codexWebsocketSession) devyreObserveUpstream(conn *websocket.Conn, eventType string, payload []byte) {
	if s == nil || conn == nil {
		return
	}
	switch eventType {
	case "response.created", "response.completed", "response.done", "response.incomplete", "response.failed", "error":
	default:
		return
	}
	id := gjson.GetBytes(payload, "response.id").String()
	r := &s.devyre
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != conn {
		r.resetLocked(conn)
	}
	if eventType == "response.created" {
		if r.running != id {
			// Upstream runs one response at a time on a socket: a new one means the
			// previous one is no longer running.
			r.endRunningLocked()
			r.running = id
		}
		return
	}
	if id != "" && id != r.running {
		// The end of a response not followed as running, such as a queued create's
		// rejection: it never ran, so it cannot be interrupted.
		r.rememberEndedLocked(id, false)
		return
	}
	// The running response ended. An "error" event names no response; it ends the
	// running one too, so an interrupt is never written after it.
	r.endRunningLocked()
}

// devyreInterruptedEnd reports whether payload, a response.incomplete read from conn,
// ends a response this router interrupted on that socket. Such a response may end
// before producing any output, with zero output tokens, which is the interrupt's
// answer and not an upstream that silently gave up.
func (s *codexWebsocketSession) devyreInterruptedEnd(conn *websocket.Conn, payload []byte) bool {
	if s == nil || conn == nil {
		return false
	}
	if gjson.GetBytes(payload, "response.incomplete_details.reason").String() != "interrupted" {
		return false
	}
	id := gjson.GetBytes(payload, "response.id").String()
	r := &s.devyre
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != conn || id == "" {
		return false
	}
	if r.running == id {
		return r.runningInterrupted
	}
	for _, ended := range r.ended {
		if ended.id == id {
			return ended.interrupted
		}
	}
	return false
}

// devyreLookupSession returns the execution session without creating one.
func (e *CodexWebsocketsExecutor) devyreLookupSession(sessionID string) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil || sessionID == "" {
		return nil
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.sessions[sessionID]
}

// DevyreInterruptResponse writes a client's response.interrupt, unchanged, to the
// upstream socket of the execution session when the response it names runs there, on
// the credential that socket belongs to. Otherwise it writes nothing and says why.
func (e *CodexWebsocketsExecutor) DevyreInterruptResponse(ctx context.Context, sessionID string, payload []byte) (cliproxyexecutor.DevyreInterruptOutcome, error) {
	sess := e.devyreLookupSession(sessionID)
	if sess == nil {
		return cliproxyexecutor.DevyreInterruptNotInProgress, nil
	}
	sess.connMu.Lock()
	conn, authID, wsURL := sess.conn, sess.authID, sess.wsURL
	sess.connMu.Unlock()
	return e.devyreInterruptOnConn(ctx, sess, conn, authID, wsURL, payload)
}

// DevyreInterruptResponse forwards to the websocket executor; HTTP responses have no
// socket to interrupt.
func (e *CodexAutoExecutor) DevyreInterruptResponse(ctx context.Context, sessionID string, payload []byte) (cliproxyexecutor.DevyreInterruptOutcome, error) {
	if e == nil || e.wsExec == nil {
		return cliproxyexecutor.DevyreInterruptNotInProgress, nil
	}
	return e.wsExec.DevyreInterruptResponse(ctx, sessionID, payload)
}

func (e *CodexWebsocketsExecutor) devyreInterruptOnConn(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, authID string, wsURL string, payload []byte) (cliproxyexecutor.DevyreInterruptOutcome, error) {
	responseID := gjson.GetBytes(payload, "response_id")
	if sess == nil || conn == nil || responseID.Type != gjson.String || responseID.String() == "" {
		return cliproxyexecutor.DevyreInterruptNotInProgress, nil
	}
	if sess.upstreamDisconnectError(conn) != nil {
		return cliproxyexecutor.DevyreInterruptNotInProgress, nil
	}
	id := responseID.String()
	r := &sess.devyre
	r.mu.Lock()
	state := r.stateLocked(conn, id)
	r.mu.Unlock()
	switch state {
	case devyreResponseEnded:
		return cliproxyexecutor.DevyreInterruptEnded, nil
	case devyreResponseUnknown:
		return cliproxyexecutor.DevyreInterruptNotInProgress, nil
	}
	// Like response.steer, a control frame is never written to a disabled account.
	if !cliproxyexecutor.WebsocketAuthEnabled(ctx, authID) {
		return cliproxyexecutor.DevyreInterruptNotInProgress, cliproxyexecutor.ErrDevyreInterruptCredentialDisabled
	}
	// Mark it before writing: upstream may end the response before the write returns.
	r.mu.Lock()
	if r.stateLocked(conn, id) == devyreResponseRunning {
		r.runningInterrupted = true
	}
	r.mu.Unlock()
	if errWrite := writeCodexWebsocketMessage(sess, conn, payload); errWrite != nil {
		return cliproxyexecutor.DevyreInterruptNotInProgress, mapCodexWebsocketWriteError(sess, conn, errWrite)
	}
	log.Infof("codex websockets: request forwarded session=%s auth=%s url=%s event=response.interrupt", sess.sessionID, authID, wsURL)
	return cliproxyexecutor.DevyreInterruptForwarded, nil
}

// devyreDuplexInterrupt handles a response.interrupt that reached a duplex stream's
// input. The handler's socket reader takes every response.interrupt before it reaches
// that input, so this only keeps a frame that does arrive from being rejected: it is
// treated exactly like one the reader takes. A local answer goes out as a stream event.
func (e *CodexWebsocketsExecutor) devyreDuplexInterrupt(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, authID string, wsURL string, payload []byte, out chan<- cliproxyexecutor.StreamChunk, fail func(error)) bool {
	outcome, errInterrupt := e.devyreInterruptOnConn(ctx, sess, conn, authID, wsURL, payload)
	if errInterrupt != nil {
		fail(errInterrupt)
		return false
	}
	reply := cliproxyexecutor.DevyreInterruptReply(outcome, payload)
	if reply == nil {
		return true
	}
	select {
	case out <- cliproxyexecutor.StreamChunk{Payload: reply}:
		return true
	case <-ctx.Done():
		return false
	}
}
