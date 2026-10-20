package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// devyreFakeUpstream is a Codex websocket upstream that a test scripts frame by frame.
// Its timeouts only bound a failing test; ordering comes from the protocol itself.
type devyreFakeUpstream struct {
	t           *testing.T
	server      *httptest.Server
	connections atomic.Int32
	done        chan struct{}
}

func newDevyreFakeUpstream(t *testing.T, script func(read func() []byte, write func(string))) *devyreFakeUpstream {
	t.Helper()
	u := &devyreFakeUpstream{t: t, done: make(chan struct{})}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.connections.Add(1) > 1 {
			t.Errorf("unexpected second upstream connection")
			return
		}
		defer close(u.done)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		read := func() []byte {
			_, b, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("upstream read: %v", errRead)
			}
			return b
		}
		write := func(s string) {
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(s)); errWrite != nil {
				t.Errorf("upstream write: %v", errWrite)
			}
		}
		script(read, write)
		// Keep the socket until the client closes it; nothing is replayed.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *devyreFakeUpstream) waitClosed() {
	u.t.Helper()
	select {
	case <-u.done:
	case <-time.After(5 * time.Second):
		u.t.Fatal("upstream socket was not closed")
	}
}

func devyreTestExecutor(steering bool) *CodexWebsocketsExecutor {
	exec := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{ResponseSteering: steering}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	return exec
}

func devyreTestAuth(id string, upstream *devyreFakeUpstream) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.server.URL, "websockets": "true"}}
}

func devyreTestOptions(sessionID string) core.Options {
	return core.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: sessionID}}
}

// next returns the next event of a stream, failing on an error chunk or a closed stream.
func devyreNextChunk(t *testing.T, ctx context.Context, chunks <-chan core.StreamChunk) []byte {
	t.Helper()
	select {
	case chunk, ok := <-chunks:
		if !ok {
			t.Fatal("stream ended early")
		}
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		return chunk.Payload
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func devyreExpectEvent(t *testing.T, payload []byte, eventType, responseID string) {
	t.Helper()
	if gjson.GetBytes(payload, "type").String() != eventType || gjson.GetBytes(payload, "response.id").String() != responseID {
		t.Fatalf("expected %s for %s, got %s", eventType, responseID, payload)
	}
}

func devyreDrainEnded(t *testing.T, ctx context.Context, chunks <-chan core.StreamChunk) {
	t.Helper()
	select {
	case chunk, ok := <-chunks:
		if ok {
			t.Fatalf("stream went on after its terminal event: %s %v", chunk.Payload, chunk.Err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

const devyreInterruptedR1 = `{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"interrupted"},"output":[],"usage":{"input_tokens":12,"output_tokens":0,"total_tokens":12}}}`

// The interrupt reaches the upstream socket the response runs on, byte for byte. The
// response it ends without output is the interrupt's answer, not an empty-response
// failure, so the socket stays and takes the next request.
func TestDevyreInterruptForwardsUnchangedAndKeepsTheSocket(t *testing.T) {
	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","extension":{"kept":true}}`)
	accepted := `{"type":"response.interrupt.accepted","response_id":"r1","sequence_number":3}`
	upstream := newDevyreFakeUpstream(t, func(read func() []byte, write func(string)) {
		if got := read(); gjson.GetBytes(got, "type").String() != "response.create" {
			t.Errorf("first frame: %s", got)
		}
		write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
		if got := read(); !bytes.Equal(got, interrupt) {
			t.Errorf("interrupt changed on the way: %s", got)
		}
		write(accepted)
		write(devyreInterruptedR1)
		next := read()
		if gjson.GetBytes(next, "type").String() != "response.create" || gjson.GetBytes(next, "previous_response_id").String() != "r1" {
			t.Errorf("next request: %s", next)
		}
		write(`{"type":"response.created","response":{"id":"r2","output":[]}}`)
		write(`{"type":"response.completed","response":{"id":"r2","output":[]}}`)
	})
	exec := devyreTestExecutor(false)
	auth := devyreTestAuth("interrupt-forward", upstream)
	ctx, cancel := context.WithTimeout(core.WithDownstreamWebsocket(context.Background()), 10*time.Second)
	defer cancel()
	const sessionID = "interrupt-forward"
	defer exec.CloseExecutionSession(sessionID)

	first, err := exec.ExecuteStream(ctx, auth, core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[]}`)}, devyreTestOptions(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, first.Chunks), "response.created", "r1")
	outcome, err := exec.DevyreInterruptResponse(ctx, sessionID, interrupt)
	if err != nil || outcome != core.DevyreInterruptForwarded {
		t.Fatalf("interrupt = %v, %v; want forwarded", outcome, err)
	}
	if got := devyreNextChunk(t, ctx, first.Chunks); string(got) != accepted {
		t.Fatalf("acknowledgement changed: %s", got)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, first.Chunks), "response.incomplete", "r1")
	devyreDrainEnded(t, ctx, first.Chunks)

	second, err := exec.ExecuteStream(core.WithRequiredUpstreamWebsocket(ctx), auth, core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","previous_response_id":"r1","input":[]}`)}, devyreTestOptions(sessionID))
	if err != nil {
		t.Fatalf("the interrupted response's socket was not kept: %v", err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, second.Chunks), "response.created", "r2")
	devyreExpectEvent(t, devyreNextChunk(t, ctx, second.Chunks), "response.completed", "r2")
	exec.CloseExecutionSession(sessionID)
	upstream.waitClosed()
}

// An interrupt for a response that already ended, or that the socket never ran, is not
// written upstream: between requests nobody would read upstream's answer for the client.
func TestDevyreInterruptWithNothingInFlightWritesNothing(t *testing.T) {
	upstream := newDevyreFakeUpstream(t, func(read func() []byte, write func(string)) {
		read()
		write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
		write(`{"type":"response.completed","response":{"id":"r1","output":[]}}`)
		// The next frame proves no interrupt was written before it.
		if next := read(); gjson.GetBytes(next, "type").String() != "response.create" {
			t.Errorf("an interrupt with nothing in flight reached upstream: %s", next)
		}
		write(`{"type":"response.created","response":{"id":"r2","output":[]}}`)
		write(`{"type":"response.completed","response":{"id":"r2","output":[]}}`)
	})
	exec := devyreTestExecutor(false)
	auth := devyreTestAuth("interrupt-idle", upstream)
	ctx, cancel := context.WithTimeout(core.WithDownstreamWebsocket(context.Background()), 10*time.Second)
	defer cancel()
	const sessionID = "interrupt-idle"
	defer exec.CloseExecutionSession(sessionID)

	if outcome, err := exec.DevyreInterruptResponse(ctx, sessionID, []byte(`{"type":"response.interrupt","response_id":"r1"}`)); err != nil || outcome != core.DevyreInterruptNotInProgress {
		t.Fatalf("interrupt before any session = %v, %v", outcome, err)
	}
	first, err := exec.ExecuteStream(ctx, auth, core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[]}`)}, devyreTestOptions(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, first.Chunks), "response.created", "r1")
	devyreExpectEvent(t, devyreNextChunk(t, ctx, first.Chunks), "response.completed", "r1")
	devyreDrainEnded(t, ctx, first.Chunks)

	for _, tc := range []struct {
		payload string
		want    core.DevyreInterruptOutcome
		reply   string
	}{
		{`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`, core.DevyreInterruptEnded, `{"type":"response.interrupt.accepted","response_id":"r1"}`},
		{`{"type":"response.interrupt","response_id":"r1"}`, core.DevyreInterruptEnded, `{"type":"response.interrupt.accepted","response_id":"r1"}`},
		{`{"type":"response.interrupt","response_id":"unknown"}`, core.DevyreInterruptNotInProgress, `{"type":"response.interrupt.failed","response_id":"unknown","error":{"type":"invalid_request_error","code":"response_not_in_progress","message":"No response with this id is in progress on this connection."}}`},
		{`{"type":"response.interrupt"}`, core.DevyreInterruptNotInProgress, `{"type":"response.interrupt.failed","error":{"type":"invalid_request_error","code":"response_not_in_progress","message":"No response with this id is in progress on this connection."}}`},
	} {
		outcome, errInterrupt := exec.DevyreInterruptResponse(ctx, sessionID, []byte(tc.payload))
		if errInterrupt != nil || outcome != tc.want {
			t.Fatalf("%s = %v, %v; want %v", tc.payload, outcome, errInterrupt, tc.want)
		}
		if reply := core.DevyreInterruptReply(outcome, []byte(tc.payload)); string(reply) != tc.reply {
			t.Fatalf("%s answered %s, want %s", tc.payload, reply, tc.reply)
		}
	}

	second, err := exec.ExecuteStream(core.WithRequiredUpstreamWebsocket(ctx), auth, core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","previous_response_id":"r1","input":[]}`)}, devyreTestOptions(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, second.Chunks), "response.created", "r2")
	devyreExpectEvent(t, devyreNextChunk(t, ctx, second.Chunks), "response.completed", "r2")
	// The second response ended too: its interrupt is not written either.
	if outcome, errInterrupt := exec.DevyreInterruptResponse(ctx, sessionID, []byte(`{"type":"response.interrupt","response_id":"r2"}`)); errInterrupt != nil || outcome != core.DevyreInterruptEnded {
		t.Fatalf("interrupt after the second response = %v, %v", outcome, errInterrupt)
	}
	exec.CloseExecutionSession(sessionID)
	upstream.waitClosed()
}

// Like response.steer, no control frame goes to a credential that is no longer enabled.
func TestDevyreInterruptRefusedOnADisabledCredential(t *testing.T) {
	upstream := newDevyreFakeUpstream(t, func(read func() []byte, write func(string)) {
		read()
		write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
		if got := read(); gjson.GetBytes(got, "mode").String() != "allowed" {
			t.Errorf("the refused interrupt reached upstream: %s", got)
		}
		write(devyreInterruptedR1)
	})
	exec := devyreTestExecutor(false)
	auth := devyreTestAuth("interrupt-disabled", upstream)
	ctx, cancel := context.WithTimeout(core.WithDownstreamWebsocket(context.Background()), 10*time.Second)
	defer cancel()
	const sessionID = "interrupt-disabled"
	defer exec.CloseExecutionSession(sessionID)

	stream, err := exec.ExecuteStream(ctx, auth, core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[]}`)}, devyreTestOptions(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.created", "r1")
	disabled := core.WithWebsocketAuthCheck(ctx, func(authID string) bool { return authID != auth.ID })
	outcome, err := exec.DevyreInterruptResponse(disabled, sessionID, []byte(`{"type":"response.interrupt","response_id":"r1","mode":"refused"}`))
	if !errors.Is(err, core.ErrDevyreInterruptCredentialDisabled) || outcome == core.DevyreInterruptForwarded {
		t.Fatalf("disabled credential interrupt = %v, %v", outcome, err)
	}
	// The same response on an enabled credential: the upstream's first interrupt is this one.
	if outcome, err = exec.DevyreInterruptResponse(ctx, sessionID, []byte(`{"type":"response.interrupt","response_id":"r1","mode":"allowed"}`)); err != nil || outcome != core.DevyreInterruptForwarded {
		t.Fatalf("enabled credential interrupt = %v, %v", outcome, err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.incomplete", "r1")
	devyreDrainEnded(t, ctx, stream.Chunks)
	exec.CloseExecutionSession(sessionID)
	upstream.waitClosed()
}

// A response that ends without output after this router interrupted it passes through;
// one nobody interrupted is still the empty-response failure it was.
func TestDevyreInterruptedEndIsOnlyTheRoutersOwn(t *testing.T) {
	sess := &codexWebsocketSession{}
	conn, other := &websocket.Conn{}, &websocket.Conn{}
	sess.devyreObserveUpstream(conn, "response.created", []byte(`{"type":"response.created","response":{"id":"r1"}}`))
	if sess.devyreInterruptedEnd(conn, []byte(devyreInterruptedR1)) {
		t.Fatal("an end nobody asked for was taken as the interrupt's answer")
	}
	sess.devyre.mu.Lock()
	sess.devyre.runningInterrupted = true
	sess.devyre.mu.Unlock()
	sess.devyreObserveUpstream(conn, "response.incomplete", []byte(devyreInterruptedR1))
	if !sess.devyreInterruptedEnd(conn, []byte(devyreInterruptedR1)) {
		t.Fatal("the interrupted response's end was not recognised")
	}
	if sess.devyreInterruptedEnd(other, []byte(devyreInterruptedR1)) {
		t.Fatal("another socket's response was taken as interrupted")
	}
	if sess.devyreInterruptedEnd(conn, []byte(`{"type":"response.incomplete","response":{"id":"r1","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`)) {
		t.Fatal("an incomplete response for another reason was exempted")
	}
	// A new socket starts over.
	sess.devyreObserveUpstream(other, "response.created", []byte(`{"type":"response.created","response":{"id":"r9"}}`))
	if sess.devyreInterruptedEnd(conn, []byte(devyreInterruptedR1)) {
		t.Fatal("state of a replaced socket was kept")
	}
	// The bounded history keeps the latest responses.
	for i := 0; i < devyreEndedResponsesKept+3; i++ {
		id := string(rune('a' + i))
		sess.devyreObserveUpstream(other, "response.created", []byte(`{"type":"response.created","response":{"id":"`+id+`"}}`))
		sess.devyreObserveUpstream(other, "response.completed", []byte(`{"type":"response.completed","response":{"id":"`+id+`"}}`))
	}
	sess.devyre.mu.Lock()
	kept := len(sess.devyre.ended)
	state := sess.devyre.stateLocked(other, string(rune('a'+devyreEndedResponsesKept+2)))
	sess.devyre.mu.Unlock()
	if kept != devyreEndedResponsesKept || state != devyreResponseEnded {
		t.Fatalf("ended history = %d entries, latest state %v", kept, state)
	}
}

// With steering on, the duplex owns the socket for its whole life. An interrupt that
// reaches its input is forwarded unchanged like one the handler's reader takes; the
// interrupted response's end settles the duplex, which then takes the next create; an
// interrupt for a response that ended is answered on the stream and not written.
func TestDevyreDuplexInterruptSettlesAndTakesTheNextCreate(t *testing.T) {
	interrupt1 := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
	interrupt2 := []byte(`{"type":"response.interrupt","response_id":"r2","mode":"discard_partial_items"}`)
	upstream := newDevyreFakeUpstream(t, func(read func() []byte, write func(string)) {
		read()
		write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
		if got := read(); !bytes.Equal(got, interrupt1) {
			t.Errorf("duplex interrupt changed: %s", got)
		}
		write(`{"type":"response.interrupt.accepted","response_id":"r1"}`)
		write(devyreInterruptedR1)
		// The stale interrupt for r1 is answered locally, so the create comes next.
		create := read()
		if gjson.GetBytes(create, "type").String() != "response.create" || gjson.GetBytes(create, "previous_response_id").String() != "r1" {
			t.Errorf("create after the interrupt: %s", create)
		}
		write(`{"type":"response.created","response":{"id":"r2","output":[]}}`)
		if got := read(); !bytes.Equal(got, interrupt2) {
			t.Errorf("reader-path interrupt changed: %s", got)
		}
		write(`{"type":"response.interrupt.accepted","response_id":"r2"}`)
		write(`{"type":"response.incomplete","response":{"id":"r2","status":"incomplete","incomplete_details":{"reason":"interrupted"},"output":[]}}`)
		if create := read(); gjson.GetBytes(create, "type").String() != "response.create" {
			t.Errorf("create after the second interrupt: %s", create)
		}
		write(`{"type":"response.created","response":{"id":"r3","output":[]}}`)
		write(`{"type":"response.completed","response":{"id":"r3","output":[]}}`)
	})
	exec := devyreTestExecutor(true)
	auth := devyreTestAuth("interrupt-duplex", upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := make(chan core.WebsocketInput, 8)
	ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)
	const sessionID = "interrupt-duplex"
	defer exec.CloseExecutionSession(sessionID)

	stream, err := exec.ExecuteStream(ctx, auth, core.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[]}`)}, devyreTestOptions(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.created", "r1")
	input <- core.WebsocketInput{Payload: interrupt1}
	if got := devyreNextChunk(t, ctx, stream.Chunks); gjson.GetBytes(got, "type").String() != "response.interrupt.accepted" {
		t.Fatalf("acknowledgement: %s", got)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.incomplete", "r1")
	input <- core.WebsocketInput{Payload: interrupt1}
	if got := devyreNextChunk(t, ctx, stream.Chunks); string(got) != `{"type":"response.interrupt.accepted","response_id":"r1"}` {
		t.Fatalf("stale interrupt answer: %s", got)
	}
	input <- core.WebsocketInput{Payload: []byte(`{"type":"response.create","previous_response_id":"r1","input":[]}`)}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.created", "r2")
	// The handler's reader path: the executor writes to the duplex's socket directly.
	if outcome, errInterrupt := exec.DevyreInterruptResponse(ctx, sessionID, interrupt2); errInterrupt != nil || outcome != core.DevyreInterruptForwarded {
		t.Fatalf("reader-path interrupt = %v, %v", outcome, errInterrupt)
	}
	if got := devyreNextChunk(t, ctx, stream.Chunks); gjson.GetBytes(got, "type").String() != "response.interrupt.accepted" {
		t.Fatalf("second acknowledgement: %s", got)
	}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.incomplete", "r2")
	input <- core.WebsocketInput{Payload: []byte(`{"type":"response.create","previous_response_id":"r2","input":[]}`)}
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.created", "r3")
	devyreExpectEvent(t, devyreNextChunk(t, ctx, stream.Chunks), "response.completed", "r3")
	cancel()
	for range stream.Chunks {
	}
	upstream.waitClosed()
}
