package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestDevyreTransientStoreStreaks(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	store := &devyreTransientStore{}

	// A streak is soft for its first minute of failures, a failure at least every
	// minute keeping it going.
	for _, step := range []struct {
		at   time.Duration
		soft bool
	}{{0, true}, {30 * time.Second, true}, {59 * time.Second, true}, {61 * time.Second, false}, {100 * time.Second, false}} {
		if got := store.failed("a", "claude-opus-5-5", at(step.at), at(step.at+time.Minute)); got != step.soft {
			t.Fatalf("failure at %v: soft = %v, want %v", step.at, got, step.soft)
		}
	}
	// A quiet minute starts a new streak.
	if !store.failed("a", "claude-opus-5-5", at(100*time.Second+61*time.Second), at(4*time.Minute)) {
		t.Fatal("a failure after a quiet minute did not start a new, soft streak")
	}
	// New placements go elsewhere until the latest avoidance, on that model only;
	// an empty model asks about any.
	if until, ok := store.avoidedUntil("a", "claude-opus-5-5", at(3*time.Minute)); !ok || !until.Equal(at(4*time.Minute)) {
		t.Fatalf("avoidedUntil = %v, %v; want %v", until, ok, at(4*time.Minute))
	}
	if _, ok := store.avoidedUntil("a", "claude-sonnet-5-5", at(3*time.Minute)); ok {
		t.Fatal("a failure on one model avoided the credential for another")
	}
	if _, ok := store.avoidedUntil("a", "", at(3*time.Minute)); !ok {
		t.Fatal("the any-model view missed the avoidance")
	}
	if _, ok := store.avoidedUntil("a", "claude-opus-5-5", at(4*time.Minute)); ok {
		t.Fatal("the avoidance outlasted its end")
	}
	// A success ends the streak and the avoidance.
	store.succeeded("a", "claude-opus-5-5")
	if _, ok := store.avoidedUntil("a", "", at(3*time.Minute)); ok {
		t.Fatal("a success left the avoidance in place")
	}
	if !store.failed("a", "claude-opus-5-5", at(3*time.Minute+time.Second), time.Time{}) {
		t.Fatal("a failure after a success did not start a new, soft streak")
	}
}

func TestDevyreTransientStoreIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	store := &devyreTransientStore{}
	for i := 0; i < devyreMaxTransientEntries+10; i++ {
		store.failed("auth-"+strconv.Itoa(i), "m", now, now.Add(time.Minute))
	}
	if n := len(store.entries); n > devyreMaxTransientEntries {
		t.Fatalf("the store holds %d entries, more than %d", n, devyreMaxTransientEntries)
	}
}

// softTestSignals renders a Claude 7-day window the way collectQuotaSignals stores
// the response headers, relative to the real clock the conductor runs on.
func softTestSignals(remaining float64, resetsIn time.Duration) map[string]string {
	prefix := "Anthropic-Ratelimit-Unified-7d-"
	return map[string]string{
		prefix + "Utilization": strconv.FormatFloat((100-remaining)/100, 'f', -1, 64),
		prefix + "Reset":       strconv.FormatInt(time.Now().Add(resetsIn).Unix(), 10),
		prefix + "Status":      "allowed",
	}
}

// softRig is a manager whose routing makes transient failures soft: session
// affinity over expiring-first placement. Credential a is the more urgent one, so
// a new session is placed there first.
type softRig struct {
	*transientRig
	store *devyreTransientStore
}

func newSoftRig(t *testing.T, name string, credentials int) *softRig {
	t.Helper()
	previousCooldown := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousCooldown) })

	store := &devyreTransientStore{}
	rig := &transientRig{t: t, executor: &transientTestExecutor{steps: map[string][]transientStep{}}}
	expiring := &ExpiringFirstSelector{GateRemainingPercent: 2, HeadroomShortPercent: 20, HeadroomLongPercent: 5, Focus: routingfocus.NewStore(), transients: store}
	rig.selector = NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: expiring, TTL: time.Hour})
	t.Cleanup(rig.selector.Stop)
	rig.manager = NewManager(nil, rig.selector, nil)
	rig.manager.RegisterExecutor(rig.executor)
	rig.manager.devyre.transients = store
	rig.manager.devyre.transientWait = func(ctx context.Context, d time.Duration) error {
		rig.waits = append(rig.waits, d)
		return ctx.Err()
	}
	rig.a, rig.b = "soft-"+name+"-a", "soft-"+name+"-b"
	ids := []string{rig.a, rig.b}[:credentials]
	for i, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: transientTestModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		// a: 40 % of its week left, resetting in 3 hours; b: 90 %, in 6 days.
		signals := softTestSignals(40, 3*time.Hour)
		if i == 1 {
			signals = softTestSignals(90, 6*24*time.Hour)
		}
		auth := &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{ObservedAt: time.Now(), Signals: signals}}
		if _, err := rig.manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	return &softRig{transientRig: rig, store: store}
}

func (r *softRig) served() []string {
	r.t.Helper()
	return strings.Split(r.executor.servedBy(), ",")
}

// lastServed is the credential the latest attempt went to.
func (r *softRig) lastServed() string {
	served := r.served()
	return served[len(served)-1]
}

// A 529 on one request leaves the other sessions on the credential: it is not
// cooled down, so the session bound there keeps its account and its cache, and
// only new sessions go elsewhere for a while.
func TestDevyreSoftTransientKeepsTheSessionsBoundToTheCredential(t *testing.T) {
	rig := newSoftRig(t, "kept", 2)
	rig.bind("first")

	// A new session is placed on a, the most urgent, which is overloaded.
	rig.executor.queue(rig.a, transientStep{err: customStatusError{code: 529, msg: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}})
	resp, err := rig.execute(context.Background(), "second")
	if err != nil || !strings.Contains(string(resp.Payload), rig.b) {
		t.Fatalf("Execute() = %s, %v; want the other credential's answer", resp.Payload, err)
	}
	if got, want := rig.executor.servedBy(), strings.Join([]string{rig.a, rig.a, rig.b}, ","); got != want {
		t.Fatalf("served by %s, want %s", got, want)
	}
	if rig.cooling(rig.a) {
		t.Fatal("one request's transient failure cooled the credential down for its sessions")
	}

	// A new session goes to b while a's failure is fresh, though a is more urgent.
	if _, err := rig.execute(context.Background(), "third"); err != nil || rig.lastServed() != rig.b {
		t.Fatalf("a new session went to %s (%v), want %s", rig.lastServed(), err, rig.b)
	}

	// The session bound to a stays there.
	if _, err := rig.execute(context.Background(), "first"); err != nil || rig.lastServed() != rig.a {
		t.Fatalf("the bound session's next request went to %s (%v), want %s", rig.lastServed(), err, rig.a)
	}
	if bound := rig.boundTo("first"); bound != rig.a {
		t.Fatalf("the bound session is bound to %s, want %s", bound, rig.a)
	}

	// a answered, so it takes new sessions again.
	if _, err := rig.execute(context.Background(), "fourth"); err != nil || rig.lastServed() != rig.a {
		t.Fatalf("a new session after a answered went to %s (%v), want %s", rig.lastServed(), err, rig.a)
	}
}

// A provider with one credential answers again at once: the client's retry after
// a transient failure reaches the credential instead of a router that has none to
// offer for a minute.
func TestDevyreSoftTransientKeepsALoneCredentialAnswering(t *testing.T) {
	rig := newSoftRig(t, "lone", 1)
	rig.bind("only")
	rig.executor.queue(rig.a, transientStep{err: statusError(http.StatusBadGateway)}, transientStep{err: statusError(http.StatusBadGateway)}, transientStep{err: statusError(http.StatusBadGateway)})
	if _, err := rig.execute(context.Background(), "only"); err == nil {
		t.Fatal("a request that failed on the only credential succeeded")
	}
	resp, err := rig.execute(context.Background(), "only")
	if err != nil || !strings.Contains(string(resp.Payload), rig.a) {
		t.Fatalf("the retry: Execute() = %s, %v; want the credential's answer", resp.Payload, err)
	}
	if bound := rig.boundTo("only"); bound != rig.a {
		t.Fatalf("the session is bound to %s, want %s", bound, rig.a)
	}
}

// A request that tries every account during an overload leaves its session where
// its cache is, not on the last account it tried.
func TestDevyreSoftTransientKeepsASessionThatTriedEveryAccount(t *testing.T) {
	rig := newSoftRig(t, "every", 2)
	rig.bind("tried")
	overloaded := transientStep{err: customStatusError{code: 529, msg: "Overloaded"}}
	rig.executor.queue(rig.a, overloaded, overloaded, overloaded)
	rig.executor.queue(rig.b, overloaded)
	if _, err := rig.execute(context.Background(), "tried"); err == nil {
		t.Fatal("a request that failed on every credential succeeded")
	}
	if got, want := rig.executor.servedBy(), strings.Join([]string{rig.a, rig.a, rig.a, rig.a, rig.b}, ","); got != want {
		t.Fatalf("served by %s, want %s", got, want)
	}
	if bound := rig.boundTo("tried"); bound != rig.a {
		t.Fatalf("the session is bound to %s, want %s, where its cache is", bound, rig.a)
	}
	// Once the overload is over, it goes on where it was.
	if _, err := rig.execute(context.Background(), "tried"); err != nil || rig.lastServed() != rig.a {
		t.Fatalf("the next request went to %s (%v), want %s", rig.lastServed(), err, rig.a)
	}
}

// After a minute of nothing but failures the credential is in trouble of its own:
// it cools down and its sessions move, as upstream does.
func TestDevyreSoftTransientEndsAfterAMinuteOfFailures(t *testing.T) {
	rig := newSoftRig(t, "minute", 2)
	rig.bind("moved")
	now := time.Now()
	rig.store.entries = map[string]devyreTransientEntry{
		devyreTransientKey(rig.a, transientTestModel): {first: now.Add(-61 * time.Second), last: now.Add(-time.Second)},
	}
	rig.executor.queue(rig.a, transientStep{err: statusError(http.StatusServiceUnavailable)}, transientStep{err: statusError(http.StatusServiceUnavailable)}, transientStep{err: statusError(http.StatusServiceUnavailable)})
	resp, err := rig.execute(context.Background(), "moved")
	if err != nil || !strings.Contains(string(resp.Payload), rig.b) {
		t.Fatalf("Execute() = %s, %v; want the other credential's answer", resp.Payload, err)
	}
	if !rig.cooling(rig.a) {
		t.Fatal("a credential failing for over a minute did not cool down")
	}
	if bound := rig.boundTo("moved"); bound != rig.b {
		t.Fatalf("the session is bound to %s, want %s", bound, rig.b)
	}
}

// A success ends the streak: new sessions are placed on the credential again.
func TestDevyreSoftTransientEndsWithASuccess(t *testing.T) {
	rig := newSoftRig(t, "success", 2)
	rig.bind("bound")
	rig.executor.queue(rig.a, transientStep{err: statusError(http.StatusBadGateway)})
	if _, err := rig.execute(context.Background(), "bound"); err != nil || rig.lastServed() != rig.a {
		t.Fatalf("the bound session's request: %v on %s, want an answer from %s after a retry", err, rig.lastServed(), rig.a)
	}
	if _, ok := rig.store.avoidedUntil(rig.a, transientTestModel, time.Now()); ok {
		t.Fatal("the credential is still avoided after it answered")
	}
	if _, err := rig.execute(context.Background(), "new"); err != nil || rig.lastServed() != rig.a {
		t.Fatalf("a new session went to %s (%v), want %s", rig.lastServed(), err, rig.a)
	}
}

// A stream that fails after its first payload reached the client cools nothing
// and keeps its session: the client's retry reaches the same credential.
func TestDevyreSoftTransientKeepsAStreamsSession(t *testing.T) {
	rig := newSoftRig(t, "stream", 2)
	rig.bind("streamed")
	rig.executor.queue(rig.a, transientStep{midErr: customStatusError{code: 529, msg: "Overloaded"}})
	stream := func() (string, error) {
		opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"streamed"}}}
		result, err := rig.manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: transientTestModel}, opts)
		return drainStream(t, result, err)
	}
	if _, err := stream(); err == nil {
		t.Fatal("a stream that failed after its first payload ended without an error")
	}
	if rig.cooling(rig.a) {
		t.Fatal("the failed stream cooled the credential down")
	}
	payload, err := stream()
	if err != nil || !strings.Contains(payload, rig.a) {
		t.Fatalf("the retry streamed %q, %v; want the bound credential's answer", payload, err)
	}
	if bound := rig.boundTo("streamed"); bound != rig.a {
		t.Fatalf("the session is bound to %s, want %s", bound, rig.a)
	}
}

// Under another placement than expiring-first nothing places new sessions away
// from a failing credential, so a transient failure cools it as upstream does.
func TestDevyreSoftTransientNeedsExpiringFirst(t *testing.T) {
	rig := newTransientRig(t, "round-robin-529")
	rig.executor.queue(rig.a, transientStep{err: customStatusError{code: 529, msg: "Overloaded"}})
	if _, err := rig.execute(context.Background(), "fresh"); err != nil {
		t.Fatal(err)
	}
	if !rig.cooling(rig.a) {
		t.Fatal("under round-robin placement a transient failure did not cool the credential")
	}
}

// The selector places a new session away from a credential that just failed, and
// on it when every candidate did; a failing focus takes no sessions over.
func TestExpiringFirstLeavesOutTransientlyFailingCredentials(t *testing.T) {
	t.Parallel()
	store := &devyreTransientStore{}
	selector := newHeadroomTestSelector()
	selector.transients = store
	urgent := efClaudeAuth("tf-urgent", efWindow{"7d", 40, 3 * efHour})
	roomy := efClaudeAuth("tf-roomy", efWindow{"7d", 90, 6 * efDay})
	pick := func(auths ...*Auth) string {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil {
			t.Fatalf("Pick() = %+v, %v", got, err)
		}
		return got.ID
	}
	if got := pick(urgent, roomy); got != urgent.ID {
		t.Fatalf("without failures the pick is %s, want %s", got, urgent.ID)
	}
	store.failed(urgent.ID, "claude-opus-5-5", efTestNow, efTestNow.Add(time.Minute))
	if got := pick(urgent, roomy); got != roomy.ID {
		t.Fatalf("with the urgent credential failing the pick is %s, want %s", got, roomy.ID)
	}
	store.failed(roomy.ID, "claude-opus-5-5", efTestNow, efTestNow.Add(time.Minute))
	if got := pick(urgent, roomy); got != urgent.ID {
		t.Fatalf("with every credential failing the pick is %s, want %s by urgency", got, urgent.ID)
	}

	focus := routingfocus.NewStore()
	selector.Focus = focus
	focusedID := "tf-focused"
	if err := focus.Set(routingfocus.Focus{AuthID: focusedID, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	focused := efClaudeAuth(focusedID, efWindow{"7d", 90, 6 * efDay})
	if got := selector.usableFocus(context.Background(), "claude", "claude-opus-5-5", []*Auth{focused, urgent}); len(got) != 1 {
		t.Fatalf("usableFocus() = %d auths, want the focus", len(got))
	}
	store.failed(focusedID, "claude-opus-5-5", efTestNow, efTestNow.Add(time.Minute))
	if got := selector.usableFocus(context.Background(), "claude", "claude-opus-5-5", []*Auth{focused, urgent}); len(got) != 0 {
		t.Fatal("a focus that just failed would take sessions over")
	}
}
