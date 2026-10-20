package openai

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// What Codex 0.163 does on a steer: interrupt the response it is reading, read until
// that response ends, then continue on the same socket with previous_response_id. The
// interrupt reaches the upstream socket at once and unchanged, no "error" comes back, the
// interrupted response's empty end is passed through, and the next create runs on the
// same upstream socket.
func TestDevyreResponsesInterruptReachesTheUpstreamSocket(t *testing.T) {
	devyreBothModes(t, func(t *testing.T, steering bool) {
		interrupt := `{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`
		h := newDevyreSocketHarness(t, steering, "forward-"+devyreModeName(steering), func(u devyreUpstreamConn) {
			if create := u.read(); gjson.GetBytes(create, "type").String() != "response.create" {
				t.Errorf("first frame: %s", create)
			}
			u.write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
			u.write(`{"type":"response.in_progress","response":{"id":"r1","output":[]}}`)
			// The response keeps running until the interrupt arrives, and ends without output.
			if got := u.read(); !bytes.Equal(got, []byte(interrupt)) {
				t.Errorf("interrupt changed or missing: %s", got)
			}
			u.write(`{"type":"response.interrupt.accepted","response_id":"r1","sequence_number":3}`)
			u.write(`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"interrupted"},"output":[],"usage":{"input_tokens":9,"output_tokens":0,"total_tokens":9}}}`)
			next := u.read()
			if gjson.GetBytes(next, "type").String() != "response.create" || gjson.GetBytes(next, "previous_response_id").String() != "r1" {
				t.Errorf("follow-up: %s", next)
			}
			u.write(`{"type":"response.created","response":{"id":"r2","output":[]}}`)
			u.write(`{"type":"response.completed","response":{"id":"r2","output":[]}}`)
			u.waitClosed()
		})
		h.send(`{"type":"response.create","model":"` + h.model + `","input":[]}`)
		h.expect("response.created", "r1")
		h.expect("response.in_progress", "r1")
		h.send(interrupt)
		h.expect("response.interrupt.accepted", "r1")
		h.expect("response.incomplete", "r1")
		h.send(`{"type":"response.create","model":"` + h.model + `","previous_response_id":"r1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"steer"}]}]}`)
		h.expect("response.created", "r2")
		h.expect("response.completed", "r2")
		_ = h.client.Close()
		h.waitUpstreamClosed()
		if h.connections.Load() != 1 {
			t.Fatalf("upstream connections = %d", h.connections.Load())
		}
	})
}

// An interrupt that arrives after its response ended, the race Codex is in whenever the
// response finishes while it decides to interrupt, is answered here without an error and
// never written upstream; neither is one for a response the socket never ran.
func TestDevyreResponsesInterruptWithNothingInFlight(t *testing.T) {
	devyreBothModes(t, func(t *testing.T, steering bool) {
		h := newDevyreSocketHarness(t, steering, "idle-"+devyreModeName(steering), func(u devyreUpstreamConn) {
			u.read()
			u.write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
			u.write(`{"type":"response.completed","response":{"id":"r1","output":[]}}`)
			// The next frame proves no interrupt was written before it.
			if next := u.read(); gjson.GetBytes(next, "type").String() != "response.create" {
				t.Errorf("an interrupt with nothing in flight reached upstream: %s", next)
			}
			u.write(`{"type":"response.created","response":{"id":"r2","output":[]}}`)
			u.write(`{"type":"response.completed","response":{"id":"r2","output":[]}}`)
			u.waitClosed()
		})
		// Before any response, too.
		h.send(`{"type":"response.interrupt","response_id":"r0"}`)
		if reply := h.expect("response.interrupt.failed", "r0"); gjson.GetBytes(reply, "error.code").String() != "response_not_in_progress" {
			t.Fatalf("reply: %s", reply)
		}
		h.send(`{"type":"response.create","model":"` + h.model + `","input":[]}`)
		h.expect("response.created", "r1")
		h.expect("response.completed", "r1")
		h.send(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
		if reply := h.expect("response.interrupt.accepted", "r1"); gjson.GetBytes(reply, "error").Exists() {
			t.Fatalf("reply: %s", reply)
		}
		h.send(`{"type":"response.interrupt","response_id":"unknown","mode":"discard_partial_items"}`)
		h.expect("response.interrupt.failed", "unknown")
		h.send(`{"type":"response.create","model":"` + h.model + `","previous_response_id":"r1","input":[]}`)
		h.expect("response.created", "r2")
		h.expect("response.completed", "r2")
		_ = h.client.Close()
		h.waitUpstreamClosed()
		if h.connections.Load() != 1 {
			t.Fatalf("upstream connections = %d", h.connections.Load())
		}
	})
}

// Like response.steer, an interrupt is never written to a credential that is no longer
// enabled, and the connection fails so the client starts over on an enabled one.
func TestDevyreResponsesInterruptRefusedOnADisabledCredential(t *testing.T) {
	devyreBothModes(t, func(t *testing.T, steering bool) {
		h := newDevyreSocketHarness(t, steering, "disabled-"+devyreModeName(steering), func(u devyreUpstreamConn) {
			u.read()
			u.write(`{"type":"response.created","response":{"id":"r1","output":[]}}`)
			u.waitClosed()
		})
		h.send(`{"type":"response.create","model":"` + h.model + `","input":[]}`)
		h.expect("response.created", "r1")
		disabled := h.auth.Clone()
		disabled.Disabled = true
		disabled.Status = coreauth.StatusDisabled
		if _, err := h.manager.Update(context.Background(), disabled); err != nil {
			t.Fatal(err)
		}
		h.send(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
		_, payload, err := h.client.ReadMessage()
		if err == nil {
			t.Fatalf("the disabled credential's connection stayed usable: %s", payload)
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("the interrupt was neither refused nor answered")
		}
		h.waitUpstreamClosed()
		if h.frames.Load() != 1 {
			t.Fatalf("the disabled credential got %d frames, want only the create", h.frames.Load())
		}
	})
}
