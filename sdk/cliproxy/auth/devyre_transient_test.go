package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// transientStep is what the fake executor answers to one attempt.
type transientStep struct {
	err     error // the call fails before any response
	bootErr error // ExecuteStream: the stream fails before its first payload
	midErr  error // ExecuteStream: the stream fails after its first payload
}

// transientTestExecutor answers each credential from its queue of steps, then succeeds.
type transientTestExecutor struct {
	mu     sync.Mutex
	steps  map[string][]transientStep
	served []string
}

func (e *transientTestExecutor) Identifier() string { return "claude" }

func (e *transientTestExecutor) next(authID string) transientStep {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.served = append(e.served, authID)
	queue := e.steps[authID]
	if len(queue) == 0 {
		return transientStep{}
	}
	e.steps[authID] = queue[1:]
	return queue[0]
}

func (e *transientTestExecutor) queue(authID string, steps ...transientStep) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.steps[authID] = append(e.steps[authID], steps...)
}

func (e *transientTestExecutor) servedBy() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.served, ",")
}

func (e *transientTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if step := e.next(auth.ID); step.err != nil {
		return cliproxyexecutor.Response{}, step.err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":"` + auth.ID + `"}`)}, nil
}

func (e *transientTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *transientTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	step := e.next(auth.ID)
	if step.err != nil {
		return nil, step.err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	switch {
	case step.bootErr != nil:
		chunks <- cliproxyexecutor.StreamChunk{Err: step.bootErr}
	case step.midErr != nil:
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first from " + auth.ID)}
		chunks <- cliproxyexecutor.StreamChunk{Err: step.midErr}
	default:
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("all from " + auth.ID)}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *transientTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *transientTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

// transientRig is a manager with two Claude credentials behind session affinity.
type transientRig struct {
	t        *testing.T
	manager  *Manager
	selector *SessionAffinitySelector
	executor *transientTestExecutor
	waits    []time.Duration
	onWait   func()
	a, b     string
}

const transientTestModel = "claude-opus-5-5"

func newTransientRig(t *testing.T, name string) *transientRig {
	t.Helper()
	previousCooldown := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousCooldown) })

	rig := &transientRig{t: t, executor: &transientTestExecutor{steps: map[string][]transientStep{}}}
	rig.selector = NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	t.Cleanup(rig.selector.Stop)
	rig.manager = NewManager(nil, rig.selector, nil)
	rig.manager.RegisterExecutor(rig.executor)
	rig.manager.devyre.transientWait = func(ctx context.Context, d time.Duration) error {
		rig.waits = append(rig.waits, d)
		if rig.onWait != nil {
			rig.onWait()
		}
		return ctx.Err()
	}
	rig.a, rig.b = "tr-"+name+"-a", "tr-"+name+"-b"
	for _, id := range []string{rig.a, rig.b} {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: transientTestModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := rig.manager.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

// execute runs one request of session ("" for none) through the manager.
func (r *transientRig) execute(ctx context.Context, session string) (cliproxyexecutor.Response, error) {
	opts := cliproxyexecutor.Options{}
	if session != "" {
		opts.Headers = http.Header{"X-Claude-Code-Session-Id": []string{session}}
	}
	return r.manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: transientTestModel}, opts)
}

// bind makes session's first request, which binds it to the first credential.
func (r *transientRig) bind(session string) {
	r.t.Helper()
	if _, err := r.execute(context.Background(), session); err != nil {
		r.t.Fatal(err)
	}
	if got := r.executor.servedBy(); got != r.a {
		r.t.Fatalf("the first request was served by %s, want %s", got, r.a)
	}
}

func (r *transientRig) boundTo(session string) string {
	authID, status := r.selector.LookupAffinity("mixed", transientTestModel, "claude:"+session)
	if status != "bound" {
		return status
	}
	return authID
}

func (r *transientRig) cooling(authID string) bool {
	current, ok := r.manager.GetByID(authID)
	if !ok {
		return false
	}
	blocked, _, _ := isAuthBlockedForModel(current, transientTestModel, time.Now())
	return blocked
}

func statusError(code int) error {
	return customStatusError{code: code, msg: fmt.Sprintf("upstream answered %d", code)}
}

// A transient failure on the bound credential retries that credential: the
// session keeps its binding and its account's cache, and the credential cools
// down for nothing.
func TestDevyreTransientRetriesTheBoundCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"502 from the edge", customStatusError{code: http.StatusBadGateway, msg: "<html><head><title>502 Bad Gateway</title></head></html>"}},
		{"500", statusError(http.StatusInternalServerError)},
		{"503", statusError(http.StatusServiceUnavailable)},
		{"504", statusError(http.StatusGatewayTimeout)},
		{"529 overloaded", customStatusError{code: 529, msg: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}},
		{"idle connection closed", fmt.Errorf(`Post "https://api.anthropic.com/v1/messages?beta=true": %w`, errors.New("http: server closed idle connection"))},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransientRig(t, strings.ReplaceAll(tc.name, " ", "-"))
			rig.bind("kept")
			rig.executor.queue(rig.a, transientStep{err: tc.err})
			resp, err := rig.execute(context.Background(), "kept")
			if err != nil || !strings.Contains(string(resp.Payload), rig.a) {
				t.Fatalf("Execute() = %s, %v; want the bound credential's answer", resp.Payload, err)
			}
			if got, want := rig.executor.servedBy(), strings.Join([]string{rig.a, rig.a, rig.a}, ","); got != want {
				t.Fatalf("served by %s, want %s", got, want)
			}
			if fmt.Sprint(rig.waits) != "[500ms]" {
				t.Fatalf("waits = %v, want one of 500ms", rig.waits)
			}
			if bound := rig.boundTo("kept"); bound != rig.a {
				t.Fatalf("the session is bound to %s, want %s", bound, rig.a)
			}
			if rig.cooling(rig.a) {
				t.Fatal("a transient failure cooled the credential down")
			}
		})
	}
}

// The retries are bounded: after two, the session fails over as before.
func TestDevyreTransientRetriesAreBounded(t *testing.T) {
	hook := efLogHook(t, log.InfoLevel)
	rig := newTransientRig(t, "bounded")
	rig.bind("moved")
	rig.executor.queue(rig.a, transientStep{err: statusError(502)}, transientStep{err: statusError(502)}, transientStep{err: statusError(502)})
	hook.Reset()
	resp, err := rig.execute(context.Background(), "moved")
	if err != nil || !strings.Contains(string(resp.Payload), rig.b) {
		t.Fatalf("Execute() = %s, %v; want the other credential's answer", resp.Payload, err)
	}
	if got, want := rig.executor.servedBy(), strings.Join([]string{rig.a, rig.a, rig.a, rig.a, rig.b}, ","); got != want {
		t.Fatalf("served by %s, want %s", got, want)
	}
	if fmt.Sprint(rig.waits) != "[500ms 1s]" {
		t.Fatalf("waits = %v, want 500ms then 1s", rig.waits)
	}
	if bound := rig.boundTo("moved"); bound != rig.b {
		t.Fatalf("the session is bound to %s, want %s", bound, rig.b)
	}
	var retries, failovers int
	for _, line := range efAffinityLogs(hook) {
		switch {
		case strings.HasPrefix(line, "session-affinity: transient failure on the bound auth, retrying it | session=claude:moved auth="+rig.a+" provider=claude model=claude-opus-5-5 retry="):
			retries++
		case strings.HasPrefix(line, "session-affinity: bound auth failed, reselected | session=claude:moved from="+rig.a+" auth="+rig.b+" "):
			failovers++
		}
	}
	if retries != 2 || failovers != 1 {
		t.Fatalf("logged %d retries and %d failovers, want 2 and 1: %q", retries, failovers, efAffinityLogs(hook))
	}
}

// Failures that are the credential's own move the session as before.
func TestDevyreTransientLeavesCredentialFailuresAlone(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			rig := newTransientRig(t, fmt.Sprint(code))
			rig.bind("own")
			rig.executor.queue(rig.a, transientStep{err: statusError(code)})
			_, _ = rig.execute(context.Background(), "own")
			if len(rig.waits) != 0 {
				t.Fatalf("waited %v for a %d", rig.waits, code)
			}
			if got := rig.executor.servedBy(); strings.Count(got, rig.a) != 2 {
				t.Fatalf("served by %s, want the bound credential tried once", got)
			}
		})
	}
}

// Sessionless requests and a new session's first request have no cache to keep:
// they move on at once, as before.
func TestDevyreTransientKeepsTodaysBehaviorWithoutABinding(t *testing.T) {
	t.Run("sessionless", func(t *testing.T) {
		rig := newTransientRig(t, "sessionless")
		rig.executor.queue(rig.a, transientStep{err: statusError(502)})
		resp, err := rig.execute(context.Background(), "")
		if err != nil || !strings.Contains(string(resp.Payload), rig.b) || len(rig.waits) != 0 {
			t.Fatalf("Execute() = %s, %v after waits %v; want the other credential at once", resp.Payload, err, rig.waits)
		}
	})
	t.Run("new session", func(t *testing.T) {
		rig := newTransientRig(t, "new-session")
		rig.executor.queue(rig.a, transientStep{err: statusError(502)})
		resp, err := rig.execute(context.Background(), "fresh")
		if err != nil || !strings.Contains(string(resp.Payload), rig.b) || len(rig.waits) != 0 {
			t.Fatalf("Execute() = %s, %v after waits %v; want the other credential at once", resp.Payload, err, rig.waits)
		}
	})
}

// A credential that became unusable during the wait is not retried.
func TestDevyreTransientStopsForACredentialThatCooledDown(t *testing.T) {
	rig := newTransientRig(t, "cooled")
	rig.bind("cooled")
	rig.executor.queue(rig.a, transientStep{err: statusError(503)})
	rig.onWait = func() {
		rig.manager.MarkResult(context.Background(), Result{AuthID: rig.a, Provider: "claude", Model: transientTestModel, Success: false, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}})
	}
	resp, err := rig.execute(context.Background(), "cooled")
	if err != nil || !strings.Contains(string(resp.Payload), rig.b) {
		t.Fatalf("Execute() = %s, %v; want the other credential", resp.Payload, err)
	}
	if got, want := rig.executor.servedBy(), strings.Join([]string{rig.a, rig.a, rig.b}, ","); got != want {
		t.Fatalf("served by %s, want %s", got, want)
	}
}

// A request cancelled while it waits for a retry ends there.
func TestDevyreTransientStopsWhenTheRequestEnds(t *testing.T) {
	rig := newTransientRig(t, "cancelled")
	rig.bind("cancelled")
	rig.executor.queue(rig.a, transientStep{err: statusError(502)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.onWait = cancel
	if _, err := rig.execute(ctx, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
	if got, want := rig.executor.servedBy(), strings.Join([]string{rig.a, rig.a}, ","); got != want {
		t.Fatalf("served by %s, want %s", got, want)
	}
}

func TestDevyreTransientRetriesCountTokens(t *testing.T) {
	rig := newTransientRig(t, "count")
	rig.bind("counted")
	rig.executor.queue(rig.a, transientStep{err: statusError(503)})
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"counted"}}}
	resp, err := rig.manager.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: transientTestModel}, opts)
	if err != nil || !strings.Contains(string(resp.Payload), rig.a) || fmt.Sprint(rig.waits) != "[500ms]" {
		t.Fatalf("ExecuteCount() = %s, %v after waits %v; want the bound credential after one retry", resp.Payload, err, rig.waits)
	}
}

// drainStream reads a stream to its end: its payloads and its first error.
func drainStream(t *testing.T, result *cliproxyexecutor.StreamResult, err error) (string, error) {
	t.Helper()
	if err != nil {
		return "", err
	}
	var payloads []string
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil && streamErr == nil {
			streamErr = chunk.Err
		}
		if len(chunk.Payload) > 0 {
			payloads = append(payloads, string(chunk.Payload))
		}
	}
	return strings.Join(payloads, "|"), streamErr
}

// A stream is retried only before its first payload reached the client.
func TestDevyreTransientRetriesStreamsBeforeTheirFirstPayload(t *testing.T) {
	stream := func(rig *transientRig, session string) (string, error) {
		opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{session}}, Stream: true}
		result, err := rig.manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: transientTestModel}, opts)
		return drainStream(rig.t, result, err)
	}
	t.Run("start fails", func(t *testing.T) {
		rig := newTransientRig(t, "stream-start")
		rig.bind("streamed")
		rig.executor.queue(rig.a, transientStep{err: statusError(502)})
		got, err := stream(rig, "streamed")
		if err != nil || got != "all from "+rig.a || fmt.Sprint(rig.waits) != "[500ms]" {
			t.Fatalf("stream = %q, %v after waits %v; want the bound credential after one retry", got, err, rig.waits)
		}
		if bound := rig.boundTo("streamed"); bound != rig.a {
			t.Fatalf("the session is bound to %s, want %s", bound, rig.a)
		}
	})
	t.Run("stream fails before its first payload", func(t *testing.T) {
		rig := newTransientRig(t, "stream-bootstrap")
		rig.bind("streamed")
		rig.executor.queue(rig.a, transientStep{bootErr: errors.New("read tcp 10.0.0.2:51234->160.79.104.10:443: wsarecv: An existing connection was forcibly closed by the remote host.")})
		got, err := stream(rig, "streamed")
		if err != nil || got != "all from "+rig.a || fmt.Sprint(rig.waits) != "[500ms]" {
			t.Fatalf("stream = %q, %v after waits %v; want the bound credential after one retry", got, err, rig.waits)
		}
	})
	t.Run("start and bootstrap share the budget", func(t *testing.T) {
		rig := newTransientRig(t, "stream-budget")
		rig.bind("streamed")
		rig.executor.queue(rig.a, transientStep{err: statusError(529)}, transientStep{bootErr: errors.New("unexpected EOF")}, transientStep{err: statusError(502)})
		got, err := stream(rig, "streamed")
		if err != nil || got != "all from "+rig.b || fmt.Sprint(rig.waits) != "[500ms 1s]" {
			t.Fatalf("stream = %q, %v after waits %v; want the other credential after two retries", got, err, rig.waits)
		}
	})
	t.Run("stream fails after its first payload", func(t *testing.T) {
		rig := newTransientRig(t, "stream-mid")
		rig.bind("streamed")
		rig.executor.queue(rig.a, transientStep{midErr: errors.New("unexpected EOF")})
		got, err := stream(rig, "streamed")
		if got != "first from "+rig.a || err == nil || len(rig.waits) != 0 {
			t.Fatalf("stream = %q, %v after waits %v; want the first payload, then the error, and no retry", got, err, rig.waits)
		}
		if served := rig.executor.servedBy(); served != rig.a+","+rig.a {
			t.Fatalf("served by %s, want no second attempt once output was sent", served)
		}
	})
}

func TestDevyreTransientFailureKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{statusError(500), true},
		{statusError(502), true},
		{statusError(503), true},
		{statusError(504), true},
		{statusError(529), true},
		{statusError(401), false},
		{statusError(403), false},
		{statusError(408), false},
		{statusError(429), false},
		{statusError(501), false},
		{errors.New("http: server closed idle connection"), true},
		{errors.New("unexpected EOF"), true},
		{errors.New("dial tcp: lookup api.anthropic.com: no such host"), true},
		{fmt.Errorf("stream: %w", errors.New(`{"type":"error","error":{"type":"overloaded_error"}}`)), true},
		{context.Canceled, false},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), false},
		{errors.New("translation failed"), false},
		{nil, false},
	} {
		if got := devyreTransientFailure(tc.err); got != tc.want {
			t.Fatalf("devyreTransientFailure(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
