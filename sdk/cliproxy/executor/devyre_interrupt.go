package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

// DevyreResponseInterruptType is the Responses websocket control frame Codex sends to
// stop the response in flight: {"type":"response.interrupt","response_id":<the id
// from response.created>,"mode":"discard_partial_items"}. Codex 0.163 sends it when new
// input arrives during generation (its instant interrupt, for models whose catalog entry
// sets use_responses_lite), then keeps reading the response until it ends and sends the
// next response.create on the same socket. An "error" event read meanwhile fails the
// request being read, so the router never answers an interrupt with one.
const DevyreResponseInterruptType = "response.interrupt"

// DevyreInterruptOutcome is what became of a client's response.interrupt.
type DevyreInterruptOutcome int

const (
	// DevyreInterruptNotInProgress: no response with that id runs on the connection's
	// upstream socket, so nothing was written upstream.
	DevyreInterruptNotInProgress DevyreInterruptOutcome = iota
	// DevyreInterruptForwarded: the frame went, unchanged, to the upstream socket the
	// response runs on. Upstream answers it and ends the response.
	DevyreInterruptForwarded
	// DevyreInterruptEnded: the response already ended on the connection's upstream
	// socket (its terminal event was read), so nothing was written upstream.
	DevyreInterruptEnded
)

func (o DevyreInterruptOutcome) String() string {
	switch o {
	case DevyreInterruptForwarded:
		return "forwarded"
	case DevyreInterruptEnded:
		return "already_ended"
	default:
		return "not_in_progress"
	}
}

// ErrDevyreInterruptCredentialDisabled refuses an interrupt on a connection whose
// credential is no longer enabled. Like response.steer, the frame is not written to
// that account's socket and the connection fails, so the client reconnects.
var ErrDevyreInterruptCredentialDisabled = errors.New("websocket credential is no longer enabled")

// DevyreResponseInterrupter is an executor that can write a client's response.interrupt
// to the upstream socket of an execution session. It never dials, selects another
// credential or replays anything.
type DevyreResponseInterrupter interface {
	DevyreInterruptResponse(ctx context.Context, sessionID string, payload []byte) (DevyreInterruptOutcome, error)
}

// DevyreIsResponseInterrupt reports whether a client frame is a response.interrupt.
func DevyreIsResponseInterrupt(payload []byte) bool {
	return gjson.GetBytes(payload, "type").String() == DevyreResponseInterruptType && json.Valid(payload)
}

type devyreInterruptReply struct {
	Type       string                     `json:"type"`
	ResponseID string                     `json:"response_id,omitempty"`
	Error      *devyreInterruptReplyError `json:"error,omitempty"`
}

type devyreInterruptReplyError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DevyreInterruptReply is the frame that answers an interrupt the router did not
// forward, or nil when upstream answers it. Codex reads these acknowledgements by
// type and response_id only: a response.interrupt.accepted lets it keep the socket, a
// response.interrupt.failed makes it retire the socket when it waited for one, and it
// ignores both while draining a response. A response that already ended has stopped,
// which is all the interrupt asked for, so it is accepted; an unknown one is not.
func DevyreInterruptReply(outcome DevyreInterruptOutcome, payload []byte) []byte {
	var reply devyreInterruptReply
	switch outcome {
	case DevyreInterruptEnded:
		reply.Type = "response.interrupt.accepted"
	case DevyreInterruptNotInProgress:
		reply.Type = "response.interrupt.failed"
		reply.Error = &devyreInterruptReplyError{
			Type:    "invalid_request_error",
			Code:    "response_not_in_progress",
			Message: "No response with this id is in progress on this connection.",
		}
	default:
		return nil
	}
	if responseID := gjson.GetBytes(payload, "response_id"); responseID.Type == gjson.String && strings.TrimSpace(responseID.String()) != "" {
		reply.ResponseID = responseID.String()
	}
	out, errMarshal := json.Marshal(reply)
	if errMarshal != nil {
		return nil
	}
	return out
}
