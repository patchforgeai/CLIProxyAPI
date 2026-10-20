package openai

import (
	"context"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// devyreInterruptResponse forwards a client's response.interrupt to the upstream socket
// its response runs on, unchanged, or answers it here when that response is not running
// there (see cliproxyexecutor.DevyreInterruptReply). It never answers with an "error"
// event: Codex would read that as the failure of its next request. An error return
// fails the connection, the way a refused response.steer does.
func (h *OpenAIResponsesAPIHandler) devyreInterruptResponse(ctx context.Context, writer *responsesWebsocketWriter, sessionID string, payload []byte) error {
	outcome := cliproxyexecutor.DevyreInterruptNotInProgress
	if interrupter := h.devyreResponseInterrupter(); interrupter != nil {
		var errInterrupt error
		outcome, errInterrupt = interrupter.DevyreInterruptResponse(ctx, sessionID, payload)
		if errInterrupt != nil {
			log.Warnf("responses websocket: response.interrupt refused id=%s error=%v", sessionID, errInterrupt)
			return errInterrupt
		}
	}
	log.Infof("responses websocket: response.interrupt id=%s outcome=%s", sessionID, outcome)
	reply := cliproxyexecutor.DevyreInterruptReply(outcome, payload)
	if reply == nil {
		return nil
	}
	// No timeline: the handler loop appends to it without a lock.
	return writeResponsesWebsocketPayload(writer, nil, reply, time.Now())
}

func (h *OpenAIResponsesAPIHandler) devyreResponseInterrupter() cliproxyexecutor.DevyreResponseInterrupter {
	if h == nil || h.AuthManager == nil {
		return nil
	}
	exec, ok := h.AuthManager.Executor("codex")
	if !ok || exec == nil {
		return nil
	}
	interrupter, _ := exec.(cliproxyexecutor.DevyreResponseInterrupter)
	return interrupter
}

// devyreSessionAuthEnabled is the handler's live credential check for this connection:
// the manager's view first, then a credential staged on the execution session.
func (h *OpenAIResponsesAPIHandler) devyreSessionAuthEnabled(sessionID string) func(string) bool {
	return func(authID string) bool {
		if h == nil || h.AuthManager == nil {
			return false
		}
		auth, ok := h.AuthManager.GetByID(authID)
		if !ok || auth == nil {
			auth, ok = h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID)
		}
		return ok && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled
	}
}
