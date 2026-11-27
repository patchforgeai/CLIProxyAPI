// Package tokenrefresh is the one way a refresh token is spent at a provider's token endpoint,
// for every provider whose accounts Anvil serves (Claude and ChatGPT): one request at a time for
// each token, at most two at once for a provider, a 429 that holds the provider's next refreshes
// until its Retry-After, a replay only of an answer that says the request was not taken, and a
// failure that says what it means for the sign-in (Kind).
//
// devyre: Claude's and ChatGPT's refreshes each had their own copy of this (anthropic_auth.go,
// openai_auth.go); ChatGPT's replayed a request whose answer may have been lost, which spends a
// single-use refresh token for nothing. devyre/README.md, "A sign-in's life", has the reasoning.
package tokenrefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// attempts bounds how often one refresh sends its request: a replay follows only an answer
	// that says the request was not taken (a 5xx).
	attempts = 3
	// timeout bounds one refresh, its replays and the waits between them included. It is the
	// caller's, not its context's: a refresh whose caller goes away still lands its answer.
	timeout = 30 * time.Second
	// perProvider is how many refreshes of one provider go out at once.
	perProvider = 2
	// The hold a 429 asks for, as its Retry-After says, within these bounds.
	minHold = 5 * time.Second
	maxHold = 5 * time.Minute
	// maxExcerpt bounds the part of an answer that is not an OAuth error kept for logs.
	maxExcerpt = 200
)

// Kind is what a failed refresh means for the sign-in.
type Kind string

const (
	// The provider ended the sign-in at its time limit (Claude's "Refresh token expired",
	// OpenAI's refresh_token_expired). Terminal.
	KindEnded Kind = "ended"
	// The refresh token was already spent (OpenAI's refresh_token_reused). Terminal.
	KindReused Kind = "reused"
	// The provider revoked the sign-in (OpenAI's refresh_token_invalidated: signed out, or
	// signed in again elsewhere). Terminal.
	KindRevoked Kind = "revoked"
	// The provider put the account on hold (Claude's account_on_hold). Terminal.
	KindOnHold Kind = "on_hold"
	// Any other invalid_grant, or a 401 from the token endpoint: the provider refuses this
	// refresh token and always will. Terminal.
	KindRefused Kind = "refused"

	// The request never reached the server (DNS, dial, TLS handshake): the token is unspent.
	KindUnreachable Kind = "unreachable"
	// The request went out and no readable answer came back (a timeout, a reset, a body that
	// is not one): the provider may have spent the token.
	KindLost Kind = "lost"
	// The token endpoint answered 429.
	KindRateLimited Kind = "rate_limited"
	// The token endpoint answered 5xx.
	KindServer Kind = "server_error"
	// A Cloudflare challenge or another page in place of the token endpoint's answer.
	KindBlocked Kind = "blocked"
	// Another 4xx the provider named no grant error for.
	KindRejected Kind = "rejected"
)

// Terminal reports whether the provider will never take this refresh token again: only a new
// sign-in, or a token file another writer replaced, can bring the credential back.
func (k Kind) Terminal() bool {
	switch k {
	case KindEnded, KindReused, KindRevoked, KindOnHold, KindRefused:
		return true
	default:
		return false
	}
}

// Failure is a refresh that did not give new tokens.
type Failure struct {
	Provider string
	// Account names the credential in logs (its file name; never a token).
	Account string
	Kind    Kind
	// Status is the token endpoint's HTTP status; 0 when no answer came.
	Status int
	// Code and Description are the OAuth error the provider gave (RFC 6749 §5.2's `error` and
	// `error_description`, or OpenAI's `error.code` and `error.message`).
	Code        string
	Description string
	// Excerpt is the start of an answer that carried no OAuth error (an HTML page), for logs.
	Excerpt string
	// Wait is how long the provider asked to be left alone (a 429's Retry-After, clamped).
	Wait time.Duration
	// Held is set when no request went out because an earlier 429 still holds the provider.
	Held bool
	// Attempts is how many requests the refresh sent.
	Attempts int
	// Err is the transport or decoding error, when there is one.
	Err error
}

func (f *Failure) Error() string {
	if f == nil {
		return "token refresh failed"
	}
	if f.Held {
		return fmt.Sprintf("token refresh failed with status %d: refresh held for %s after a 429", f.Status, f.Wait.Round(time.Second))
	}
	if f.Status == 0 {
		return fmt.Sprintf("token refresh failed (%s): %v", f.Kind, f.Err)
	}
	answer := f.Answer()
	if f.Err != nil {
		return fmt.Sprintf("token refresh failed with status %d: %s: %v", f.Status, answer, f.Err)
	}
	return fmt.Sprintf("token refresh failed with status %d: %s", f.Status, answer)
}

// Answer is the provider's answer in a few words: its OAuth error and description, else the
// start of its body; for a request that got none, why (the transport's error). Never a token: a
// failed answer carries none, and a transport error names the endpoint, not the request's body.
func (f *Failure) Answer() string {
	if f == nil {
		return ""
	}
	switch {
	case f.Held:
		return fmt.Sprintf("held for %s after a 429", f.Wait.Round(time.Second))
	case f.Status == 0 && f.Err != nil:
		return excerpt([]byte(f.Err.Error()))
	case f.Code != "" && f.Description != "":
		return f.Code + ": " + f.Description
	case f.Code != "":
		return f.Code
	case f.Description != "":
		return f.Description
	case f.Excerpt != "":
		return f.Excerpt
	case f.Status != 0:
		return http.StatusText(f.Status)
	default:
		return ""
	}
}

// StatusCode is the token endpoint's HTTP status (statusCoder in the conductor).
func (f *Failure) StatusCode() int {
	if f == nil {
		return 0
	}
	return f.Status
}

// RetryAfter is how long a 429 asked to wait, when it did (retryAfterProvider in the conductor).
func (f *Failure) RetryAfter() *time.Duration {
	if f == nil || f.Wait <= 0 {
		return nil
	}
	wait := f.Wait
	return &wait
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

// replayable reports whether the same request may be sent again at once: only after a 5xx,
// whose server took nothing. A request that may have been taken (KindLost) is never replayed:
// the refresh token is single use, and a replay of a spent one is refused for good.
func (f *Failure) replayable() bool {
	return f != nil && f.Kind == KindServer
}

// AsFailure returns the Failure err holds, if any.
func AsFailure(err error) (*Failure, bool) {
	var failure *Failure
	if errors.As(err, &failure) && failure != nil {
		return failure, true
	}
	return nil, false
}

// Answer is a token endpoint's answer to a refresh.
type Answer struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	// RefreshTokenExpiresIn is how long the sign-in lasts from this answer, when the answer says
	// (Claude's does); nil when it does not.
	RefreshTokenExpiresIn *int64 `json:"refresh_token_expires_in"`
	// Body is the whole answer, for what a provider adds (Claude's account and organization).
	Body []byte `json:"-"`
	// ReceivedAt is when the answer came.
	ReceivedAt time.Time `json:"-"`
}

// ExpiresAt is when the new access token ends.
func (a *Answer) ExpiresAt() time.Time {
	return a.ReceivedAt.Add(time.Duration(a.ExpiresIn) * time.Second)
}

// SignInEndsAt is when the sign-in ends by this answer, and whether it said.
func (a *Answer) SignInEndsAt() (time.Time, bool) {
	return SignInEnd(a.ReceivedAt, a.RefreshTokenExpiresIn)
}

// SignInEnd is when a sign-in ends by an answer received at receivedAt that gave expiresIn
// seconds (`refresh_token_expires_in`), and whether it gave any.
func SignInEnd(receivedAt time.Time, expiresIn *int64) (time.Time, bool) {
	if expiresIn == nil || *expiresIn <= 0 {
		return time.Time{}, false
	}
	return receivedAt.Add(time.Duration(*expiresIn) * time.Second).UTC().Truncate(time.Second), true
}

// EndAfterRefresh is when a sign-in ends once a refresh answered (RFC 3339, empty when unknown):
// the earlier of the end it kept and the end the answer gave, as Claude Code keeps it, since a
// refresh never makes a sign-in last longer; the answer's when none was kept (a sign-in made
// before ends were kept learns its end from the first answer that gives one); the kept one when
// the answer gave none. Never a guess: an end no answer gave stays unknown.
func EndAfterRefresh(kept, answered string) string {
	keptAt, keptOK := ParseEnd(kept)
	answeredAt, answeredOK := ParseEnd(answered)
	switch {
	case keptOK && answeredOK && answeredAt.Before(keptAt):
		return FormatEnd(answeredAt)
	case keptOK:
		return FormatEnd(keptAt)
	case answeredOK:
		return FormatEnd(answeredAt)
	default:
		return ""
	}
}

// ParseEnd reads an end as the token files keep it (RFC 3339).
func ParseEnd(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil || at.IsZero() {
		return time.Time{}, false
	}
	return at, true
}

// FormatEnd writes an end as the token files keep it: RFC 3339 in UTC, to the second.
func FormatEnd(at time.Time) string {
	return at.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// Exchange is one provider's refresh request.
type Exchange struct {
	// Provider names the provider ("claude", "codex"): its refreshes share the hold and slots.
	Provider string
	// Account names the credential in logs (its file name).
	Account      string
	RefreshToken string
	// Send sends the provider's own refresh request with ctx.
	Send func(ctx context.Context) (*http.Response, error)
	// Read reads an answer's body; io.ReadAll when nil.
	Read func(*http.Response) ([]byte, error)
}

var (
	// flights are the refreshes under way, by provider and refresh token.
	flights = make(map[string]*flight)
	mu      sync.Mutex
	held    = make(map[string]time.Time)
	slots   = make(map[string]chan struct{})
	// now is the clock, and replayWait the pause before a replay (tests).
	now        = time.Now
	replayWait = func(attempt int) time.Duration { return time.Duration(attempt-1) * time.Second }
)

// Reset forgets every hold and refresh in flight: for tests, which share the package state.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	held = make(map[string]time.Time)
	slots = make(map[string]chan struct{})
	flights = make(map[string]*flight)
}

// HeldUntil is when an earlier 429 stops holding the provider's refreshes; zero when none does.
func HeldUntil(provider string) time.Time {
	mu.Lock()
	defer mu.Unlock()
	until := held[provider]
	if !until.After(now()) {
		return time.Time{}
	}
	return until
}

func hold(provider string, wait time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	until := now().Add(wait)
	if until.After(held[provider]) {
		held[provider] = until
	}
}

func slotsOf(provider string) chan struct{} {
	mu.Lock()
	defer mu.Unlock()
	s := slots[provider]
	if s == nil {
		s = make(chan struct{}, perProvider)
		slots[provider] = s
	}
	return s
}

func heldFailure(x Exchange) error {
	until := HeldUntil(x.Provider)
	if until.IsZero() {
		return nil
	}
	return &Failure{
		Provider: x.Provider,
		Account:  x.Account,
		Kind:     KindRateLimited,
		Status:   http.StatusTooManyRequests,
		Wait:     until.Sub(now()),
		Held:     true,
	}
}

// ErrNotSent: the refresh was given up before its request went out (its caller ended while it
// waited for a slot). It says nothing of the sign-in: the token is unspent.
var ErrNotSent = errors.New("token refresh not sent")

// flight is one refresh of a refresh token, which the callers asking for it meanwhile share.
type flight struct {
	done    chan struct{}
	answer  *Answer
	err     error
	notSent bool
}

// Refresh spends x's refresh token and returns the answer, or a *Failure, or an error wrapping
// ErrNotSent and the caller's context error when the caller ended before anything was sent.
//
// Callers of one token share one refresh. The one that leads it waits for a slot of the
// provider with its own context; when it gives up before sending, the others do not share that:
// each tries again to lead, so a request's deadline never fails the refresh loop's caller.
func Refresh(ctx context.Context, x Exchange) (*Answer, error) {
	if strings.TrimSpace(x.RefreshToken) == "" {
		return nil, errors.New("refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := x.Provider + "\x00" + x.RefreshToken
	for {
		if err := heldFailure(x); err != nil {
			return nil, err
		}
		mu.Lock()
		if f := flights[key]; f != nil {
			mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, notSent(ctx)
			}
			if f.notSent {
				continue // its leader gave up before sending: lead it ourselves
			}
			return f.result()
		}
		f := &flight{done: make(chan struct{})}
		flights[key] = f
		mu.Unlock()
		f.answer, f.notSent, f.err = lead(ctx, x)
		mu.Lock()
		delete(flights, key)
		mu.Unlock()
		close(f.done)
		return f.result()
	}
}

func (f *flight) result() (*Answer, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Each caller gets its own copy: they may change it.
	copied := *f.answer
	return &copied, nil
}

func notSent(ctx context.Context) error {
	return fmt.Errorf("%w: %w", ErrNotSent, ctx.Err())
}

// lead takes a slot of the provider and runs the refresh: a free slot even for a caller already
// gone (its refresh still lands), and nothing sent for one that ends while it waits.
func lead(ctx context.Context, x Exchange) (*Answer, bool, error) {
	s := slotsOf(x.Provider)
	select {
	case s <- struct{}{}:
	default:
		select {
		case s <- struct{}{}:
		case <-ctx.Done():
			return nil, true, notSent(ctx)
		}
	}
	defer func() { <-s }()
	// A 429 may have come while this one waited for a slot.
	if errHeld := heldFailure(x); errHeld != nil {
		return nil, false, errHeld
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	answer, err := run(runCtx, x)
	return answer, false, err
}

func run(ctx context.Context, x Exchange) (*Answer, error) {
	var last *Failure
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, last
			case <-time.After(replayWait(attempt)):
			}
		}
		answer, failure := exchange(ctx, x)
		if failure == nil {
			return answer, nil
		}
		failure.Attempts = attempt
		last = failure
		log.WithFields(log.Fields{"provider": x.Provider, "credential": x.Account, "kind": failure.Kind}).
			Warnf("token refresh of %s, attempt %d, failed: %v", x.Account, attempt, failure)
		if failure.Kind == KindRateLimited {
			hold(x.Provider, failure.Wait)
		}
		if !failure.replayable() {
			break
		}
	}
	return nil, last
}

func exchange(ctx context.Context, x Exchange) (*Answer, *Failure) {
	var getConn, gotConn atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) { getConn.Store(true) },
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
	})
	resp, errSend := x.Send(ctx)
	if errSend != nil {
		kind := KindLost
		if neverSent(errSend, getConn.Load(), gotConn.Load()) {
			kind = KindUnreachable
		}
		return nil, &Failure{Provider: x.Provider, Account: x.Account, Kind: kind, Err: errSend}
	}
	defer func() { _ = resp.Body.Close() }()
	read := x.Read
	if read == nil {
		read = func(r *http.Response) ([]byte, error) { return io.ReadAll(r.Body) }
	}
	body, errRead := read(resp)
	if errRead != nil {
		return nil, &Failure{Provider: x.Provider, Account: x.Account, Kind: KindLost, Status: resp.StatusCode, Err: fmt.Errorf("read the answer: %w", errRead)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, answerFailure(x, resp, body)
	}
	var answer Answer
	if errDecode := json.Unmarshal(body, &answer); errDecode != nil || strings.TrimSpace(answer.AccessToken) == "" {
		if errDecode == nil {
			errDecode = errors.New("the answer has no access token")
		}
		// A 200 that cannot be read may have rotated the token: never replayed.
		return nil, &Failure{Provider: x.Provider, Account: x.Account, Kind: KindLost, Status: resp.StatusCode, Err: fmt.Errorf("read the answer: %w", errDecode)}
	}
	answer.Body = body
	answer.ReceivedAt = now()
	if strings.TrimSpace(answer.RefreshToken) == "" {
		// The provider kept the refresh token (it rotates it only sometimes).
		answer.RefreshToken = x.RefreshToken
	}
	return &answer, nil
}

// neverSent reports whether a request that failed with err never reached the server. Through a
// real http.Transport the trace says it: it asked for a connection (getConn) and never got one
// (dial, proxy, DNS, TLS handshake). A transport that reports no trace is judged by err alone.
func neverSent(err error, getConn, gotConn bool) bool {
	if getConn {
		return !gotConn
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH)
}

// oauthError reads the OAuth error of an answer: RFC 6749's `error` and `error_description`,
// OpenAI's `error.code` and `error.message`, or a top-level `code`.
func oauthError(body []byte) (code, description string) {
	var answer map[string]any
	if json.Unmarshal(body, &answer) != nil {
		return "", ""
	}
	text := func(value any) string {
		s, _ := value.(string)
		return strings.TrimSpace(s)
	}
	switch e := answer["error"].(type) {
	case string:
		code = strings.TrimSpace(e)
	case map[string]any:
		code = text(e["code"])
		if code == "" {
			code = text(e["type"])
		}
		description = text(e["message"])
	}
	if top := text(answer["code"]); top != "" && (code == "" || code == "invalid_grant" || code == "invalid_request_error") {
		code = top
	}
	if d := text(answer["error_description"]); d != "" {
		description = d
	}
	return code, description
}

// The descriptions and codes the providers end a sign-in with, from their own clients (Claude
// Code 2.1.332, Codex 0.162.1).
const (
	claudeEnded  = "Refresh token expired"
	claudeOnHold = "account_on_hold"
)

var openAITerminal = map[string]Kind{
	"refresh_token_expired":     KindEnded,
	"refresh_token_reused":      KindReused,
	"refresh_token_invalidated": KindRevoked,
}

func answerFailure(x Exchange, resp *http.Response, body []byte) *Failure {
	failure := &Failure{Provider: x.Provider, Account: x.Account, Status: resp.StatusCode}
	failure.Code, failure.Description = oauthError(body)
	if failure.Code == "" && failure.Description == "" {
		failure.Excerpt = excerpt(body)
	}
	status := resp.StatusCode
	authStatus := status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden
	switch kind, named := openAITerminal[strings.ToLower(failure.Code)]; {
	case isChallenge(resp, body):
		failure.Kind = KindBlocked
	case authStatus && failure.Description == claudeOnHold:
		failure.Kind = KindOnHold
	case named && status != http.StatusTooManyRequests && status < 500:
		failure.Kind = kind
	case strings.EqualFold(failure.Code, "invalid_grant") && status < 500:
		failure.Kind = KindRefused
		if strings.EqualFold(failure.Description, claudeEnded) {
			failure.Kind = KindEnded
		}
	case status == http.StatusUnauthorized:
		failure.Kind = KindRefused
	case status == http.StatusTooManyRequests:
		failure.Kind = KindRateLimited
		failure.Wait = retryAfter(resp)
	case status >= 500:
		failure.Kind = KindServer
	case status == http.StatusForbidden:
		failure.Kind = KindBlocked
	default:
		failure.Kind = KindRejected
	}
	return failure
}

// isChallenge reports whether an answer is Cloudflare's page rather than the token endpoint's.
func isChallenge(resp *http.Response, body []byte) bool {
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("cf-mitigated")), "challenge") {
		return true
	}
	lower := strings.ToLower(string(body[:min(len(body), 4096)]))
	return strings.Contains(lower, "challenge-platform") ||
		(strings.Contains(lower, "just a moment") && strings.Contains(lower, "cloudflare"))
}

func excerpt(body []byte) string {
	text := strings.Join(strings.Fields(string(body[:min(len(body), 4*maxExcerpt)])), " ")
	if len(text) > maxExcerpt {
		text = text[:maxExcerpt] + "…"
	}
	return text
}

// retryAfter is the hold a 429 asks for: its Retry-After in seconds or as a date, else its
// Retry-After-Ms, within minHold and maxHold.
func retryAfter(resp *http.Response) time.Duration {
	clamp := func(d time.Duration) time.Duration {
		return max(minHold, min(maxHold, d))
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After")); raw != "" {
		if seconds, err := time.ParseDuration(raw + "s"); err == nil {
			return clamp(seconds)
		}
		if when, err := http.ParseTime(raw); err == nil {
			return clamp(when.Sub(now()))
		}
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After-Ms")); raw != "" {
		if ms, err := time.ParseDuration(raw + "ms"); err == nil {
			return clamp(ms)
		}
	}
	return minHold
}
