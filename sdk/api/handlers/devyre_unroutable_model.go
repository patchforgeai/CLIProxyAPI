package handlers

import (
	"errors"
	"net/http"
)

// UnroutableModelError is the router's own answer to a model no credential
// serves (devyre): "unknown provider for model", as the OpenAI-shaped body the
// request paths send. It is decided here, before any credential is asked, so no
// retry, reconnect or credential rotation can change it.
//
// Upstream sends it as a plain error, and the Responses websocket keeps every
// model_not_found body from the client (clienterror.IsRequestFault treats one as
// a credential's capability gap, worth rotating): it closes the socket without a
// word, the client reconnects with the same request five times and only learns
// the reason over HTTP. Its type lets the websocket tell it apart
// (shouldExposeResponsesUpstreamError) and send it at once.
type UnroutableModelError struct {
	body string
}

// NewUnroutableModelError wraps the OpenAI-shaped body of the answer.
func NewUnroutableModelError(body string) *UnroutableModelError {
	return &UnroutableModelError{body: body}
}

func (e *UnroutableModelError) Error() string { return e.body }

// StatusCode is the status the HTTP paths answer it with.
func (e *UnroutableModelError) StatusCode() int { return http.StatusBadRequest }

// IsUnroutableModel reports whether err is, or wraps, the router's own
// "unknown provider for model" answer.
func IsUnroutableModel(err error) bool {
	var unroutable *UnroutableModelError
	return errors.As(err, &unroutable)
}
