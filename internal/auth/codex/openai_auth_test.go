package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewCodexAuthDoesNotSetRequestTimeout(t *testing.T) {
	if got := NewCodexAuth(nil).httpClient.Timeout; got != 0 {
		t.Fatalf("HTTP client timeout = %s, want zero", got)
	}
}

func TestRefreshTokens_UsesIndependentTimeout(t *testing.T) {
	resetCodexRefreshGroupForTest()
	defer resetCodexRefreshGroupForTest()

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	var requestDeadline time.Time
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var ok bool
				requestDeadline, ok = req.Context().Deadline()
				if !ok {
					t.Fatal("refresh request has no deadline")
				}
				if errContext := req.Context().Err(); errContext != nil {
					t.Fatalf("refresh request context is already done: %v", errContext)
				}
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"probe"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokens(callerCtx, "codex-test.json", "independent-timeout-token")
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if requestDeadline.IsZero() || !requestDeadline.After(time.Now()) {
		t.Fatalf("refresh deadline = %v, want a future deadline", requestDeadline)
	}
}

func resetCodexRefreshGroupForTest() {
	tokenrefresh.Reset()
}

func TestRefreshTokens_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	resetCodexRefreshGroupForTest()
	defer resetCodexRefreshGroupForTest()
	var calls int32
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","code":"refresh_token_reused"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokens(context.Background(), "codex-test.json", "dummy_refresh_token")
	if err == nil {
		t.Fatalf("expected error for non-retryable refresh failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refresh_token_reused") {
		t.Fatalf("expected refresh_token_reused in error, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt, got %d", got)
	}
}

func TestRefreshTokens_DeduplicatesConcurrentRefreshAcrossInstances(t *testing.T) {
	resetCodexRefreshGroupForTest()
	t.Cleanup(resetCodexRefreshGroupForTest)

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"token_type":"Bearer",
				"expires_in":3600
			}`)),
			Header:  make(http.Header),
			Request: req,
		}, nil
	})
	authA := &CodexAuth{httpClient: &http.Client{Transport: transport}}
	authB := &CodexAuth{httpClient: &http.Client{Transport: transport}}

	results := make(chan *CodexTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func(auth *CodexAuth, launched chan<- struct{}) {
		if launched != nil {
			close(launched)
		}
		tokenData, errRefresh := auth.RefreshTokens(context.Background(), "codex-test.json", "shared-refresh-token")
		results <- tokenData
		errs <- errRefresh
	}

	go runRefresh(authA, nil)
	<-started

	secondLaunched := make(chan struct{})
	go runRefresh(authB, secondLaunched)
	<-secondLaunched
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if errRefresh := <-errs; errRefresh != nil {
			t.Fatalf("expected refresh to succeed, got %v", errRefresh)
		}
		tokenData := <-results
		if tokenData == nil || tokenData.AccessToken != "new-access" || tokenData.RefreshToken != "new-refresh" {
			t.Fatalf("unexpected token data: %#v", tokenData)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected both refresh callers to share a single upstream call, got %d", got)
	}
}

func TestNewCodexAuthWithProxyURL_OverrideDirectDisablesProxy(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://proxy.example.com:8080"}}
	auth := NewCodexAuthWithProxyURL(cfg, "direct")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected http.Transport, got %T", auth.httpClient.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewCodexAuthWithProxyURL_OverrideProxyTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	auth := NewCodexAuthWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := auth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected http.Transport, got %T", auth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://override.example.com:8081" {
		t.Fatalf("proxy URL = %v, want http://override.example.com:8081", proxyURL)
	}
}

func TestRefreshTokens_PlanTypeDefaultsToFreeWhenMissing(t *testing.T) {
	resetCodexRefreshGroupForTest()
	defer resetCodexRefreshGroupForTest()

	idTokenWithoutPlan := makeTestJWT(map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-12345",
		},
	})

	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := `{"access_token":"at-1","refresh_token":"rt-1","id_token":"` + idTokenWithoutPlan + `","token_type":"Bearer","expires_in":3600}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	tokenData, errRefresh := auth.RefreshTokens(context.Background(), "codex-test.json", "dummy-refresh")
	if errRefresh != nil {
		t.Fatalf("RefreshTokens failed: %v", errRefresh)
	}
	if tokenData == nil {
		t.Fatal("tokenData is nil")
	}
	if got := tokenData.PlanType; got != "free" {
		t.Fatalf("tokenData.PlanType = %q, want free", got)
	}
}

func TestRefreshTokens_ExtractsPlanTypeWhenPresent(t *testing.T) {
	resetCodexRefreshGroupForTest()
	defer resetCodexRefreshGroupForTest()

	idTokenWithPlan := makeTestJWT(map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-12345",
			"chatgpt_plan_type":  "pro",
		},
	})

	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := `{"access_token":"at-1","refresh_token":"rt-1","id_token":"` + idTokenWithPlan + `","token_type":"Bearer","expires_in":3600}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	tokenData, errRefresh := auth.RefreshTokens(context.Background(), "codex-test.json", "dummy-refresh")
	if errRefresh != nil {
		t.Fatalf("RefreshTokens failed: %v", errRefresh)
	}
	if tokenData == nil {
		t.Fatal("tokenData is nil")
	}
	if got := tokenData.PlanType; got != "pro" {
		t.Fatalf("tokenData.PlanType = %q, want pro", got)
	}
}

func TestCreateAndUpdateTokenStorage_PlanType(t *testing.T) {
	auth := NewCodexAuth(nil)

	// Test CreateTokenStorage with missing plan type
	bundleDefault := &CodexAuthBundle{
		TokenData: CodexTokenData{
			IDToken:     "id-tok",
			AccessToken: "acc-tok",
		},
	}
	storage := auth.CreateTokenStorage(bundleDefault)
	if storage.PlanType != "free" {
		t.Fatalf("storage.PlanType = %q, want free", storage.PlanType)
	}

	// Test UpdateTokenStorage with plan type
	auth.UpdateTokenStorage(storage, &CodexTokenData{
		IDToken:     "id-tok-2",
		AccessToken: "acc-tok-2",
		PlanType:    "team",
	})
	if storage.PlanType != "team" {
		t.Fatalf("updated storage.PlanType = %q, want team", storage.PlanType)
	}

	// Test UpdateTokenStorage with empty plan type defaults to free
	auth.UpdateTokenStorage(storage, &CodexTokenData{
		IDToken:     "id-tok-3",
		AccessToken: "acc-tok-3",
		PlanType:    "",
	})
	if storage.PlanType != "free" {
		t.Fatalf("updated storage.PlanType = %q, want free", storage.PlanType)
	}
}

// ChatGPT's refresh follows Claude's policy: a request whose answer may have been lost is never
// sent again with the same single-use token (before, it went out three times in 3 s).
func TestRefreshTokens_DoesNotReplayAfterTransportError(t *testing.T) {
	resetCodexRefreshGroupForTest()
	defer resetCodexRefreshGroupForTest()
	var calls int32
	auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("read: connection reset by peer")
	})}}
	_, err := auth.RefreshTokens(context.Background(), "codex-test.json", "single-use-refresh-token")
	failure, ok := tokenrefresh.AsFailure(err)
	if !ok || failure.Kind != tokenrefresh.KindLost {
		t.Fatalf("error = %v, want a lost answer", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one refresh attempt after an ambiguous transport error, got %d", got)
	}
}

// A 429 holds ChatGPT's refreshes until its Retry-After: the next one sends nothing.
func TestRefreshTokens_HonoursRetryAfter(t *testing.T) {
	resetCodexRefreshGroupForTest()
	defer resetCodexRefreshGroupForTest()
	var calls int32
	auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`)),
			Header:     http.Header{"Retry-After": []string{"120"}},
			Request:    req,
		}, nil
	})}}
	if _, err := auth.RefreshTokens(context.Background(), "codex-a.json", "token-a"); err == nil {
		t.Fatal("expected a 429")
	}
	_, err := auth.RefreshTokens(context.Background(), "codex-b.json", "token-b")
	failure, ok := tokenrefresh.AsFailure(err)
	if !ok || !failure.Held || failure.Wait <= 100*time.Second {
		t.Fatalf("second refresh: %v, want held for what is left of 120 s", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("requests = %d, want the 429's only", got)
	}
}

// OpenAI's terminal refresh errors, as Codex 0.162.1 names them, end the refresh at once.
func TestRefreshTokens_NamesTheEndOfTheSignIn(t *testing.T) {
	for code, want := range map[string]tokenrefresh.Kind{
		"refresh_token_expired":     tokenrefresh.KindEnded,
		"refresh_token_reused":      tokenrefresh.KindReused,
		"refresh_token_invalidated": tokenrefresh.KindRevoked,
	} {
		resetCodexRefreshGroupForTest()
		var calls int32
		auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"x","type":"invalid_request_error","code":"` + code + `"}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})}}
		_, err := auth.RefreshTokens(context.Background(), "codex-test.json", "dead-token")
		failure, ok := tokenrefresh.AsFailure(err)
		if !ok || failure.Kind != want || !failure.Kind.Terminal() || calls != 1 {
			t.Fatalf("%s: %v after %d requests, want %s after 1", code, err, calls, want)
		}
	}
	resetCodexRefreshGroupForTest()
}
