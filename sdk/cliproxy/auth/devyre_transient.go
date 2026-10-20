package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// devyre: a transient upstream failure must not move a bound session. A 502
// page from Anthropic's edge, a 529 or a connection the server closed while idle
// says nothing about the credential, so another account does not make the
// request more likely to succeed; yet the conductor retried such a request at
// once on another credential, and the session was rebound there and paid to
// write its whole context into that account's cache. A request whose session is
// bound to its credential now retries that credential first, a bounded number of
// times, before anything reached the client. The conductor's call sites are
// small hunks in conductor_execution.go and conductor_stream.go.

// devyreBoundAuthMetadataKey carries, in a request's metadata, the credential
// whose binding SessionAffinitySelector.Pick reused for the request's session:
// its own binding, or the binding of the parent a fork reads the cached prefix
// of. A new placement, a failover and a subagent with a fresh context have no
// cache to lose, and fail over at once as before.
const devyreBoundAuthMetadataKey = "devyre_bound_auth"

// devyreTransientBackoff is the wait before each retry of the bound credential:
// two retries, after half a second and after one more second.
var devyreTransientBackoff = [...]time.Duration{500 * time.Millisecond, time.Second}

// devyreTransientMarkers are transport failures, by message, that
// isTransientTransportError does not already recognize.
var devyreTransientMarkers = []string{
	"server closed idle connection",
	"http2: server sent goaway",
	"http2: client connection lost",
	"overloaded_error",
}

// devyreManagerState is the fork's own state of a Manager.
type devyreManagerState struct {
	// transientWait waits before a retry of the bound credential; a test hook,
	// nil waits on a timer.
	transientWait func(ctx context.Context, d time.Duration) error
	// transients holds the streaks of transient failures (devyre_transient_soft.go);
	// a test hook, nil uses the store the expiring-first selector shares.
	transients *devyreTransientStore
}

// devyreMarkBound tells the conductor that the request's session was already
// bound to authID: a transient failure retries that credential first.
func devyreMarkBound(metadata map[string]any, authID string) {
	if metadata != nil {
		metadata[devyreBoundAuthMetadataKey] = authID
	}
}

// devyreTransientFailure reports an upstream failure that is not the
// credential's fault and tends to pass: HTTP 500, 502, 503, 504 or 529
// (Anthropic's overloaded), or a transport failure before a response (a dial,
// DNS or TLS failure, a reset, a connection the server closed while idle). A
// request that was cancelled, timed out, or is at fault itself is not one.
func devyreTransientFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		isRequestScopedError(err) || isRequestInvalidError(err) {
		return false
	}
	switch statusCodeFromError(err) {
	case 0:
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	default:
		return false
	}
	if isTransientTransportError(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range devyreTransientMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// devyreTransient is the same-credential retry budget of one execution of a
// bound session.
type devyreTransient struct {
	m           *Manager
	provider    string
	resultModel string
	routeModel  string
	opts        cliproxyexecutor.Options
	retries     int
}

// devyreTransientRetry returns the retry budget of an execution on auth, or nil
// when the request's session is not bound to auth: sessionless requests and
// Home dispatches keep failing over at once.
func (m *Manager) devyreTransientRetry(auth *Auth, provider, resultModel, routeModel string, opts cliproxyexecutor.Options, ephemeral bool) *devyreTransient {
	if m == nil || auth == nil || ephemeral || m.HomeEnabled() {
		return nil
	}
	if bound, _ := opts.Metadata[devyreBoundAuthMetadataKey].(string); bound == "" || bound != auth.ID {
		return nil
	}
	return &devyreTransient{m: m, provider: provider, resultModel: resultModel, routeModel: routeModel, opts: opts}
}

// next decides on a retry after err failed an attempt on auth. When err is
// transient, the budget not spent and the credential still usable for the
// model (not disabled, refreshing or cooling down), it records the failure
// without a cooldown and without releasing the session's binding, waits, and
// reports true if the credential is still usable then. Otherwise err takes the
// normal failure path, which fails the session over; a request cancelled
// during the wait ends with its context's error.
func (t *devyreTransient) next(ctx context.Context, auth *Auth, err error) (bool, error) {
	if t == nil || auth == nil || !devyreTransientFailure(err) || t.retries >= len(devyreTransientBackoff) || ctx.Err() != nil || !t.usable(auth.ID) {
		return false, err
	}
	wait := devyreTransientBackoff[t.retries]
	t.retries++
	// The failure counts toward the credential's streak (devyre_transient_soft.go):
	// new sessions are placed elsewhere while it is retried.
	t.m.devyreNoteTransient(auth.ID, t.routeModel, statusCodeFromError(err), retryAfterFromError(err))
	t.m.recordAvailabilityNeutralResult(ctx, Result{AuthID: auth.ID, Provider: t.provider, Model: t.resultModel, RouteModel: t.routeModel, Success: false, Error: resultErrorFromError(err), Options: t.opts})
	session, _ := t.opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	logEntryWithRequestID(ctx).Infof("session-affinity: transient failure on the bound auth, retrying it | session=%s auth=%s provider=%s model=%s retry=%d/%d wait=%s",
		truncateSessionID(session), auth.ID, t.provider, t.routeModel, t.retries, len(devyreTransientBackoff), wait)
	if errWait := t.m.devyreWait(ctx, wait); errWait != nil {
		return false, errWait
	}
	if !t.usable(auth.ID) {
		return false, err
	}
	return true, nil
}

// usable reports whether the selectors would still pick authID for the model.
func (t *devyreTransient) usable(authID string) bool {
	current, ok := t.m.GetByID(authID)
	if !ok || current == nil {
		return false
	}
	blocked, _, _ := isAuthBlockedForModel(current, t.resultModel, time.Now())
	return !blocked
}

// devyreWait waits d or until ctx ends.
func (m *Manager) devyreWait(ctx context.Context, d time.Duration) error {
	if m.devyre.transientWait != nil {
		return m.devyre.transientWait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// devyreAttemptContext derives the context of one more upstream attempt.
func devyreAttemptContext(ctx context.Context, opts cliproxyexecutor.Options) context.Context {
	return syncMetadataSessionToContext(newUpstreamAttemptContext(ctx), opts.Metadata)
}

// devyreRetryTransient retries a transient failure (err) of a bound session's
// non-streamed execution on its own credential, through attempt. Nothing has
// reached the client yet. It returns the context of the last attempt, whose
// response headers the result is recorded from, and that attempt's outcome.
func (m *Manager) devyreRetryTransient(ctx context.Context, auth *Auth, provider, upstreamModel, resultModel, routeModel string, opts cliproxyexecutor.Options, resp cliproxyexecutor.Response, err error, attempt func(context.Context) (cliproxyexecutor.Response, error)) (context.Context, cliproxyexecutor.Response, error) {
	if err == nil {
		return ctx, resp, nil
	}
	transient := m.devyreTransientRetry(auth, provider, resultModel, routeModel, opts, false)
	for {
		retry, errNext := transient.next(ctx, auth, err)
		if !retry {
			return ctx, resp, errNext
		}
		ctx = devyreAttemptContext(ctx, opts)
		began := time.Now()
		resp, err = attempt(ctx)
		if err = markUpstreamExecutionAttemptFromContext(ctx, err); err == nil {
			return ctx, resp, nil
		}
		warnLogUpstreamFailure(ctx, nil, provider, upstreamModel, auth, time.Since(began), err)
	}
}

// retryStart retries a transient failure (err) to start a bound session's
// stream on its own credential. It returns the context of the last attempt and
// that attempt's stream or error.
func (t *devyreTransient) retryStart(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, execModel string, result *cliproxyexecutor.StreamResult, err error) (context.Context, *cliproxyexecutor.StreamResult, error) {
	for err != nil {
		retry, errNext := t.next(ctx, auth, err)
		if !retry {
			return ctx, result, errNext
		}
		ctx, result, err = t.start(ctx, executor, auth, req, opts, execModel)
	}
	return ctx, result, nil
}

// retryBootstrap retries a transient failure (err) of a bound session's stream
// before its first payload, so before anything reached the client: the failed
// stream is drained, and the next one read up to its first payload. It returns
// the context of the last attempt and that attempt's stream, buffered chunks
// and error.
func (t *devyreTransient) retryBootstrap(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, execModel string, result *cliproxyexecutor.StreamResult, buffered []cliproxyexecutor.StreamChunk, closed bool, err error) (context.Context, *cliproxyexecutor.StreamResult, []cliproxyexecutor.StreamChunk, bool, error) {
	for err != nil {
		retry, errNext := t.next(ctx, auth, err)
		if !retry {
			return ctx, result, buffered, closed, errNext
		}
		if result != nil {
			discardStreamChunks(result.Chunks)
		}
		buffered, closed = nil, false
		ctx, result, err = t.start(ctx, executor, auth, req, opts, execModel)
		if err != nil {
			result = &cliproxyexecutor.StreamResult{}
			continue
		}
		began := time.Now()
		buffered, closed, err = readStreamBootstrap(ctx, result.Chunks)
		if err = markUpstreamExecutionAttemptFromContext(ctx, err); err != nil {
			warnLogUpstreamFailure(ctx, nil, t.provider, execModel, auth, time.Since(began), err)
		}
	}
	return ctx, result, buffered, closed, nil
}

// start makes one more attempt to start the stream.
func (t *devyreTransient) start(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, execModel string) (context.Context, *cliproxyexecutor.StreamResult, error) {
	ctx = devyreAttemptContext(ctx, opts)
	began := time.Now()
	result, err := validateStreamResult(executor.ExecuteStream(ctx, auth, req, opts))
	if err = markUpstreamExecutionAttemptFromContext(ctx, err); err != nil {
		warnLogUpstreamFailure(ctx, nil, t.provider, execModel, auth, time.Since(began), err)
	}
	return ctx, result, err
}
