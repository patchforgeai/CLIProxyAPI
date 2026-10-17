package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func keepAliveStatus(t *testing.T, s *Server, secret string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/keep-alive", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	recorder := httptest.NewRecorder()
	s.engine.ServeHTTP(recorder, request)
	return recorder.Code
}

// devyre: the lifeline's /keep-alive answers only the local password and starts no idle watcher.
func TestDevyreKeepAliveProbe(t *testing.T) {
	s := newTestServerWithOptions(t, WithLocalManagementPassword("probe-password"), WithDevyreKeepAliveProbe())
	if got := keepAliveStatus(t, s, "probe-password"); got != http.StatusOK {
		t.Fatalf("with the password: status %d, want 200", got)
	}
	if got := keepAliveStatus(t, s, "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("with a wrong password: status %d, want 401", got)
	}
	if got := keepAliveStatus(t, s, ""); got != http.StatusUnauthorized {
		t.Fatalf("without a password: status %d, want 401", got)
	}
	if s.keepAliveEnabled || s.keepAliveStop != nil {
		t.Fatal("the probe started the idle watcher")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

// devyre: next to WithKeepAliveEndpoint the probe adds nothing (gin panics on a second route)
// and the idle limit stays.
func TestDevyreKeepAliveProbeWithTheIdleLimit(t *testing.T) {
	s := newTestServerWithOptions(t,
		WithLocalManagementPassword("probe-password"),
		WithKeepAliveEndpoint(time.Hour, func() {}),
		WithDevyreKeepAliveProbe(),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() {
		if err := s.Stop(ctx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()
	if !s.keepAliveEnabled {
		t.Fatal("the idle limit is gone")
	}
	if got := keepAliveStatus(t, s, "probe-password"); got != http.StatusOK {
		t.Fatalf("with the password: status %d, want 200", got)
	}
}

func TestWithoutDevyreKeepAliveProbeThereIsNoKeepAlive(t *testing.T) {
	s := newTestServerWithOptions(t, WithLocalManagementPassword("probe-password"))
	if got := keepAliveStatus(t, s, "probe-password"); got != http.StatusNotFound {
		t.Fatalf("status %d, want 404", got)
	}
}

// The supervisor computes the same value (apps/server/src/router/lifeline.test.ts pins it too).
func TestLifelineProofVector(t *testing.T) {
	got := LifelineProof("lifeline-test-password", "127.0.0.1:8317", "c2lkZWNhci1jaGFsbGVuZ2UtMDAx")
	if want := "uSzsDdDY48ZpDFJZC-a4oFnVMgYz8X6htYKCwYoUTFw"; got != want {
		t.Fatalf("LifelineProof() = %q, want %q", got, want)
	}
	for _, other := range []string{
		LifelineProof("another-password", "127.0.0.1:8317", "c2lkZWNhci1jaGFsbGVuZ2UtMDAx"),
		LifelineProof("lifeline-test-password", "127.0.0.1:8318", "c2lkZWNhci1jaGFsbGVuZ2UtMDAx"),
		LifelineProof("lifeline-test-password", "127.0.0.1:8317", "c2lkZWNhci1jaGFsbGVuZ2UtMDAy"),
	} {
		if other == got {
			t.Fatal("a proof did not change with the password, the address or the challenge")
		}
	}
}

type challengeRequest struct {
	challenges []string
	peer       string
	local      net.Addr
	header     map[string]string
}

func serveChallenge(s *Server, r challengeRequest) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/keep-alive", nil)
	request.RemoteAddr = r.peer
	for _, challenge := range r.challenges {
		request.Header.Add(LifelineChallengeHeader, challenge)
	}
	for name, value := range r.header {
		request.Header.Set(name, value)
	}
	if r.local != nil {
		request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, r.local))
	}
	recorder := httptest.NewRecorder()
	s.engine.ServeHTTP(recorder, request)
	return recorder
}

// devyre: a challenge is answered with the proof, without the password being sent, and only to a
// loopback client that is not a browser.
func TestDevyreKeepAliveProbeAnswersChallenges(t *testing.T) {
	const password = "probe-password"
	const challenge = "c2lkZWNhci1jaGFsbGVuZ2UtMDAx"
	s := newTestServerWithOptions(t, WithLocalManagementPassword(password), WithDevyreKeepAliveProbe())
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 18417}
	ok := challengeRequest{challenges: []string{challenge}, peer: "127.0.0.1:50000", local: local}

	recorder := serveChallenge(s, ok)
	if recorder.Code != http.StatusOK {
		t.Fatalf("challenge: status %d, want 200; body %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Proof  string `json:"proof"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if body.Status != "ok" || body.Proof != LifelineProof(password, "127.0.0.1:18417", challenge) {
		t.Fatalf("answer = %+v, want the proof for 127.0.0.1:18417", body)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	ipv6 := ok
	ipv6.peer, ipv6.local = "[::1]:50000", &net.TCPAddr{IP: net.IPv6loopback, Port: 18417}
	if recorder = serveChallenge(s, ipv6); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), LifelineProof(password, "[::1]:18417", challenge)) {
		t.Fatalf("IPv6 loopback: status %d body %s", recorder.Code, recorder.Body.String())
	}

	refused := map[string]struct {
		request challengeRequest
		status  int
	}{
		"too short":       {challengeRequest{challenges: []string{"c2hvcnQ"}, peer: ok.peer, local: local}, http.StatusBadRequest},
		"not base64url":   {challengeRequest{challenges: []string{"c2lkZWNhci1jaGFsbGVuZ2Ut+DAx"}, peer: ok.peer, local: local}, http.StatusBadRequest},
		"two challenges":  {challengeRequest{challenges: []string{challenge, challenge}, peer: ok.peer, local: local}, http.StatusBadRequest},
		"a LAN client":    {challengeRequest{challenges: []string{challenge}, peer: "192.168.1.20:50000", local: local}, http.StatusForbidden},
		"a browser":       {challengeRequest{challenges: []string{challenge}, peer: ok.peer, local: local, header: map[string]string{"Origin": "http://127.0.0.1:18417"}}, http.StatusForbidden},
		"no address seen": {challengeRequest{challenges: []string{challenge}, peer: ok.peer}, http.StatusServiceUnavailable},
	}
	for name, tt := range refused {
		recorder = serveChallenge(s, tt.request)
		if recorder.Code != tt.status || strings.Contains(recorder.Body.String(), `"proof"`) {
			t.Fatalf("%s: status %d body %s, want %d without a proof", name, recorder.Code, recorder.Body.String(), tt.status)
		}
	}

	// Without a challenge, /keep-alive is upstream's: the password or 401.
	if got := keepAliveStatus(t, s, password); got != http.StatusOK {
		t.Fatalf("with the password: status %d, want 200", got)
	}
	if got := keepAliveStatus(t, s, ""); got != http.StatusUnauthorized {
		t.Fatalf("without a password or a challenge: status %d, want 401", got)
	}
}

// devyre: next to WithKeepAliveEndpoint (no lifeline) /keep-alive stays upstream's: a challenge
// gets no proof.
func TestDevyreKeepAliveChallengeNeedsTheLifelineProbe(t *testing.T) {
	s := newTestServerWithOptions(t,
		WithLocalManagementPassword("probe-password"),
		WithKeepAliveEndpoint(time.Hour, func() {}),
		WithDevyreKeepAliveProbe(),
	)
	defer func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()
	recorder := serveChallenge(s, challengeRequest{
		challenges: []string{"c2lkZWNhci1jaGFsbGVuZ2UtMDAx"},
		peer:       "127.0.0.1:50000",
		local:      &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 18417},
	})
	if recorder.Code != http.StatusUnauthorized || strings.Contains(recorder.Body.String(), `"proof"`) {
		t.Fatalf("status %d body %s, want upstream's 401", recorder.Code, recorder.Body.String())
	}
}

// listenReports records WithDevyreListenReport's calls.
type listenReports struct {
	calls chan listenReport
}

type listenReport struct {
	address string
	err     error
}

func newListenReports() *listenReports {
	return &listenReports{calls: make(chan listenReport, 4)}
}

func (r *listenReports) report(address string, err error) {
	r.calls <- listenReport{address: address, err: err}
}

func (r *listenReports) next(t *testing.T) listenReport {
	t.Helper()
	select {
	case call := <-r.calls:
		return call
	case <-time.After(10 * time.Second):
		t.Fatal("Start reported nothing")
		return listenReport{}
	}
}

// devyre: Start reports the bound address once, and the proof is bound to the address a request
// really reached, through the server's own listener.
func TestDevyreListenReportAndProofOverTheNetwork(t *testing.T) {
	const password = "probe-password"
	reports := newListenReports()
	s := newTestServerWithConfig(t, &proxyconfig.Config{Host: "127.0.0.1"},
		WithLocalManagementPassword(password), WithDevyreKeepAliveProbe(), WithDevyreListenReport(reports.report))
	started := make(chan error, 1)
	go func() { started <- s.Start() }()
	defer func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
		<-started
	}()

	call := reports.next(t)
	if call.err != nil || !strings.HasPrefix(call.address, "127.0.0.1:") || strings.HasSuffix(call.address, ":0") {
		t.Fatalf("report = %+v, want the bound 127.0.0.1 address", call)
	}
	const challenge = "c2lkZWNhci1jaGFsbGVuZ2UtMDAx"
	request, errRequest := http.NewRequest(http.MethodGet, "http://"+call.address+"/keep-alive", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	request.Header.Set(LifelineChallengeHeader, challenge)
	response, errDo := http.DefaultClient.Do(request)
	if errDo != nil {
		t.Fatal(errDo)
	}
	raw, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), LifelineProof(password, call.address, challenge)) {
		t.Fatalf("status %d body %s, want the proof for %s", response.StatusCode, raw, call.address)
	}
	select {
	case extra := <-reports.calls:
		t.Fatalf("a second report: %+v", extra)
	default:
	}
}

// devyre: a port another program holds is reported with the error, and Start fails as upstream's.
func TestDevyreListenReportWhenThePortIsTaken(t *testing.T) {
	held, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = held.Close() }()
	reports := newListenReports()
	s := newTestServerWithConfig(t, &proxyconfig.Config{Host: "127.0.0.1"}, WithDevyreListenReport(reports.report))
	s.server.Addr = held.Addr().String()
	errStart := s.Start()
	if errStart == nil {
		t.Fatal("Start() succeeded on a port another listener holds")
	}
	call := reports.next(t)
	if call.address != held.Addr().String() || call.err == nil {
		t.Fatalf("report = %+v, want %s with the error", call, held.Addr())
	}
	if !strings.Contains(errStart.Error(), call.err.Error()) {
		t.Fatalf("Start() error %q does not carry the reported %q", errStart, call.err)
	}
}
