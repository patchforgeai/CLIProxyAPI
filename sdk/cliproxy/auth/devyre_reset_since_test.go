package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A reset that says when the limits started over tells a rejection made since from one before.
func TestRejectedSinceTellsARejectionAfterTheReset(t *testing.T) {
	m, id := recoveryManager(t)
	before := time.Now().Add(-time.Second)
	rejectSharedWindow(m, id, "claude-opus-5-5", "7d", 0)
	after := time.Now().Add(time.Second)
	if !m.DevyreRejectedSince(id, before) {
		t.Fatal("a rejection after the reset time was not told apart")
	}
	if m.DevyreRejectedSince(id, after) {
		t.Fatal("a rejection before the reset time was taken for a later one")
	}
	if m.DevyreRejectedSince("unknown", before) {
		t.Fatal("a credential the manager does not hold reported a rejection")
	}
	wait := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "claude-sonnet-5-5",
		RetryAfter: &wait, Error: &Error{HTTPStatus: http.StatusInternalServerError, Message: "overloaded"}})
	if m.DevyreRejectedSince(id, after) {
		t.Fatal("a failure that is no quota rejection was taken for one")
	}
}
