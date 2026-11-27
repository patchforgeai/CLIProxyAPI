package tokenrefresh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	replayWait = func(int) time.Duration { return 0 }
	m.Run()
}

func answerWith(status int, body string, header http.Header) func(context.Context) (*http.Response, error) {
	return func(context.Context) (*http.Response, error) {
		if header == nil {
			header = make(http.Header)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}, nil
	}
}

func refreshOnce(t *testing.T, provider string, send func(context.Context) (*http.Response, error)) (*Answer, *Failure, int32) {
	t.Helper()
	var calls int32
	answer, err := Refresh(context.Background(), Exchange{
		Provider:     provider,
		Account:      "account.json",
		RefreshToken: "rt-" + t.Name(),
		Send: func(ctx context.Context) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return send(ctx)
		},
	})
	if err == nil {
		return answer, nil, calls
	}
	failure, ok := AsFailure(err)
	if !ok {
		t.Fatalf("Refresh() error = %v, want a *Failure", err)
	}
	return nil, failure, calls
}

// Each answer the providers give, as Claude Code 2.1.332 and Codex 0.162.1 name them, and what
// it means for the sign-in.
func TestAnswersSayWhatTheyMeanForTheSignIn(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		status   int
		body     string
		header   http.Header
		want     Kind
		terminal bool
		calls    int32
	}{
		{"claude's time limit", "claude", 400, `{"error":"invalid_grant","error_description":"Refresh token expired"}`, nil, KindEnded, true, 1},
		{"claude's spent token", "claude", 400, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`, nil, KindRefused, true, 1},
		{"claude's account without an organization", "claude", 400, `{"error":"invalid_grant","error_description":"No organization associated with this token"}`, nil, KindRefused, true, 1},
		{"claude's account on hold", "claude", 403, `{"error":"access_denied","error_description":"account_on_hold","error_uri":"https://claude.ai/restricted"}`, nil, KindOnHold, true, 1},
		{"openai's time limit", "codex", 401, `{"error":{"message":"Your refresh token has expired.","type":"invalid_request_error","code":"refresh_token_expired"}}`, nil, KindEnded, true, 1},
		{"openai's spent token", "codex", 401, `{"error":{"code":"refresh_token_reused","message":"already used"}}`, nil, KindReused, true, 1},
		{"openai's spent token, flat", "codex", 400, `{"error":"invalid_grant","code":"refresh_token_reused"}`, nil, KindReused, true, 1},
		{"openai's revoked sign-in", "codex", 401, `{"error":{"code":"refresh_token_invalidated"}}`, nil, KindRevoked, true, 1},
		{"a 401 that names nothing", "codex", 401, `{"error":{"message":"Unauthorized"}}`, nil, KindRefused, true, 1},
		{"a 429", "claude", 429, `{"error":"rate_limited"}`, http.Header{"Retry-After": []string{"60"}}, KindRateLimited, false, 1},
		{"a 503, replayed", "claude", 503, `{"error":"overloaded"}`, nil, KindServer, false, attempts},
		{"cloudflare's challenge", "claude", 403, `<html><head><title>Just a moment...</title></head><body>cloudflare challenge-platform</body></html>`, nil, KindBlocked, false, 1},
		{"cloudflare's challenge on a 503", "codex", 503, `<html>Just a moment... cloudflare</html>`, http.Header{"Cf-Mitigated": []string{"challenge"}}, KindBlocked, false, 1},
		{"another 400", "claude", 400, `{"error":"invalid_request","error_description":"bad body"}`, nil, KindRejected, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			_, failure, calls := refreshOnce(t, tc.provider, answerWith(tc.status, tc.body, tc.header))
			if failure == nil {
				t.Fatal("Refresh() succeeded, want a failure")
			}
			if failure.Kind != tc.want || failure.Kind.Terminal() != tc.terminal {
				t.Fatalf("kind = %s (terminal %t), want %s (terminal %t); answer %q", failure.Kind, failure.Kind.Terminal(), tc.want, tc.terminal, failure.Answer())
			}
			if calls != tc.calls || failure.Attempts != int(tc.calls) {
				t.Fatalf("requests = %d (attempts %d), want %d", calls, failure.Attempts, tc.calls)
			}
			if failure.StatusCode() != tc.status {
				t.Fatalf("status = %d, want %d", failure.StatusCode(), tc.status)
			}
		})
	}
}

func TestTheConductorsClassifiersStillReadTheText(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	_, failure, _ := refreshOnce(t, "claude", answerWith(400, `{"error":"invalid_grant","error_description":"Refresh token expired"}`, nil))
	if text := failure.Error(); !strings.Contains(text, "status 400") || !strings.Contains(text, "invalid_grant") || !strings.Contains(text, "Refresh token expired") {
		t.Fatalf("Error() = %q, want the status, the code and the description", text)
	}
	_, failure, _ = refreshOnce(t, "codex", answerWith(401, `{"error":{"code":"refresh_token_reused"}}`, nil))
	if text := failure.Error(); !strings.Contains(text, "status 401") || !strings.Contains(text, "refresh_token_reused") {
		t.Fatalf("Error() = %q, want status 401 and the code", text)
	}
}

func TestAPageInPlaceOfAnAnswerIsShortInTheLog(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	page := "<html>" + strings.Repeat("x ", 5000) + "</html>"
	_, failure, _ := refreshOnce(t, "claude", answerWith(502, page, nil))
	if len(failure.Excerpt) > maxExcerpt+len("…") {
		t.Fatalf("excerpt is %d bytes, want at most %d", len(failure.Excerpt), maxExcerpt)
	}
}

// A request that never left (no connection) leaves the token unspent; one that went out and got
// no answer may have spent it, and is never sent again.
func TestARequestThatNeverLeftIsToldFromOneWhoseAnswerWasLost(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	// Nothing listens on a port just closed.
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	closedURL := "http://" + listener.Addr().String() + "/token"
	_ = listener.Close()
	client := &http.Client{}
	send := func(url string) func(context.Context) (*http.Response, error) {
		return func(ctx context.Context) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("grant_type=refresh_token"))
			if err != nil {
				return nil, err
			}
			return client.Do(req)
		}
	}
	_, failure, calls := refreshOnce(t, "claude", send(closedURL))
	if failure == nil || failure.Kind != KindUnreachable || calls != 1 {
		t.Fatalf("a refused connection: failure %+v after %d requests, want unreachable after 1", failure, calls)
	}

	// A server that reads the request and drops the connection without an answer.
	var taken int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&taken, 1)
		_, _ = io.ReadAll(r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(server.Close)
	_, failure, calls = refreshOnce(t, "claude", send(server.URL+"/token"))
	if failure == nil || failure.Kind != KindLost {
		t.Fatalf("a dropped answer: failure %+v, want lost", failure)
	}
	if calls != 1 || atomic.LoadInt32(&taken) != 1 {
		t.Fatalf("a dropped answer was sent %d times (%d taken), want once", calls, taken)
	}
}

func TestATransportWithoutATraceIsJudgedByItsError(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	_, failure, _ := refreshOnce(t, "codex", func(context.Context) (*http.Response, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "auth.openai.com"}
	})
	if failure.Kind != KindUnreachable {
		t.Fatalf("a DNS error: kind %s, want unreachable", failure.Kind)
	}
	_, failure, calls := refreshOnce(t, "codex", func(context.Context) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	})
	if failure.Kind != KindLost || calls != 1 {
		t.Fatalf("a reset: kind %s after %d requests, want lost after 1", failure.Kind, calls)
	}
}

func TestAnUnreadableSuccessIsLostAndNeverReplayed(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	_, failure, calls := refreshOnce(t, "codex", answerWith(200, `{"access_token":`, nil))
	if failure == nil || failure.Kind != KindLost || calls != 1 {
		t.Fatalf("failure %+v after %d requests, want lost after 1", failure, calls)
	}
}

// A 429 holds every refresh of its provider until its Retry-After, and none of another's.
func TestA429HoldsTheProvidersNextRefreshes(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	_, failure, _ := refreshOnce(t, "claude", answerWith(429, `{}`, http.Header{"Retry-After": []string{"90"}}))
	if failure.Wait != 90*time.Second {
		t.Fatalf("wait = %s, want 90s", failure.Wait)
	}
	var calls int32
	_, err := Refresh(context.Background(), Exchange{Provider: "claude", Account: "other.json", RefreshToken: "another-token", Send: func(context.Context) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return answerWith(200, `{"access_token":"a"}`, nil)(context.Background())
	}})
	held, ok := AsFailure(err)
	if !ok || !held.Held || held.Kind != KindRateLimited || calls != 0 {
		t.Fatalf("another account's refresh: %v after %d requests, want held without a request", err, calls)
	}
	if held.Wait <= 85*time.Second || held.Wait > 90*time.Second {
		t.Fatalf("held wait = %s, want what is left of 90s", held.Wait)
	}
	if !HeldUntil("codex").IsZero() {
		t.Fatal("ChatGPT is held by Claude's 429")
	}
	answer, _, _ := refreshOnce(t, "codex", answerWith(200, `{"access_token":"a"}`, nil))
	if answer == nil {
		t.Fatal("ChatGPT's refresh did not go out beside Claude's hold")
	}
}

func TestRetryAfterIsReadAndBounded(t *testing.T) {
	at := func(header http.Header) time.Duration { return retryAfter(&http.Response{Header: header}) }
	if got := at(http.Header{"Retry-After": []string{"1"}}); got != minHold {
		t.Fatalf("1s = %s, want the floor %s", got, minHold)
	}
	if got := at(http.Header{"Retry-After": []string{"3600"}}); got != maxHold {
		t.Fatalf("an hour = %s, want the ceiling %s", got, maxHold)
	}
	if got := at(http.Header{"Retry-After-Ms": []string{"30000"}}); got != 30*time.Second {
		t.Fatalf("30000 ms = %s, want 30s", got)
	}
	date := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	if got := at(http.Header{"Retry-After": []string{date}}); got < 110*time.Second || got > 2*time.Minute {
		t.Fatalf("a date two minutes ahead = %s", got)
	}
	if got := at(http.Header{}); got != minHold {
		t.Fatalf("none = %s, want %s", got, minHold)
	}
}

func TestTheAnswerSaysWhenTheSignInEnds(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	answer, _, _ := refreshOnce(t, "claude", answerWith(200, `{"access_token":"a","refresh_token":"b","expires_in":28800,"refresh_token_expires_in":2592000}`, nil))
	ends, ok := answer.SignInEndsAt()
	if !ok || ends.Sub(answer.ReceivedAt) < 30*24*time.Hour-time.Second || ends.Sub(answer.ReceivedAt) > 30*24*time.Hour {
		t.Fatalf("SignInEndsAt() = %s, %t, want 30 days after %s", ends, ok, answer.ReceivedAt)
	}
	if got := answer.ExpiresAt().Sub(answer.ReceivedAt); got != 8*time.Hour {
		t.Fatalf("access token lasts %s, want 8h", got)
	}
	answer, _, _ = refreshOnce(t, "codex", answerWith(200, `{"access_token":"a","expires_in":3600}`, nil))
	if _, ok := answer.SignInEndsAt(); ok {
		t.Fatal("an answer that gives no end has one")
	}
	if answer.RefreshToken != "rt-"+t.Name() {
		t.Fatalf("refresh token = %q, want the one spent, kept", answer.RefreshToken)
	}
}

func TestOneRefreshATokenAndTwoAProvider(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	var inFlight, most, calls int32
	release := make(chan struct{})
	send := func(context.Context) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		n := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&most)
			if n <= m || atomic.CompareAndSwapInt32(&most, m, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&inFlight, -1)
		return answerWith(200, `{"access_token":"a"}`, nil)(context.Background())
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		token := []string{"t1", "t1", "t2", "t3", "t4", "t5"}[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Refresh(context.Background(), Exchange{Provider: "claude", Account: token, RefreshToken: token, Send: send}); err != nil {
				t.Errorf("refresh %s: %v", token, err)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&inFlight) < perProvider && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&inFlight); got != perProvider {
		t.Fatalf("%d refreshes out at once, want %d", got, perProvider)
	}
	close(release)
	wg.Wait()
	if most != perProvider {
		t.Fatalf("at most %d out at once, want %d", most, perProvider)
	}
	if calls != 5 {
		t.Fatalf("%d requests for 5 tokens, want one a token", calls)
	}
}

func TestARefreshOutlivesItsCaller(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var deadline time.Time
	_, err := Refresh(ctx, Exchange{Provider: "claude", Account: "a", RefreshToken: "t", Send: func(req context.Context) (*http.Response, error) {
		if req.Err() != nil {
			t.Fatalf("the request's context is done: %v", req.Err())
		}
		deadline, _ = req.Deadline()
		return answerWith(200, `{"access_token":"a"}`, nil)(req)
	}})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if deadline.IsZero() || deadline.Sub(time.Now()) > timeout {
		t.Fatalf("deadline = %s, want within %s", deadline, timeout)
	}
}

func TestARefreshNeverMakesASignInLastLonger(t *testing.T) {
	const early, late = "2026-12-01T10:00:00Z", "2026-12-20T10:00:00Z"
	cases := []struct{ kept, answered, want string }{
		{early, late, early},
		{late, early, early},
		{"", late, late},   // a sign-in made before ends were kept learns its end
		{early, "", early}, // an answer that gives none keeps the one kept
		{"", "", ""},       // never a guess
		{"not a time", "", ""},
	}
	for _, tc := range cases {
		if got := EndAfterRefresh(tc.kept, tc.answered); got != tc.want {
			t.Fatalf("EndAfterRefresh(%q, %q) = %q, want %q", tc.kept, tc.answered, got, tc.want)
		}
	}
}
