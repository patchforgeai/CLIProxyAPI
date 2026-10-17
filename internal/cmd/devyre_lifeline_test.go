package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

// syncBuffer collects what the server writes from its own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLifelineReport sends the listening line to a buffer for the test's duration.
func captureLifelineReport(t *testing.T) *syncBuffer {
	t.Helper()
	report := &syncBuffer{}
	previous := lifelineReport
	lifelineReport = report
	t.Cleanup(func() { lifelineReport = previous })
	return report
}

func TestReadPasswordLine(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "a line", input: "s3cret\n", want: "s3cret"},
		{name: "CRLF", input: "s3cret\r\n", want: "s3cret"},
		{name: "the last line without a newline", input: "s3cret", want: "s3cret"},
		{name: "inner spaces stay", input: " s3 cret \n", want: " s3 cret "},
		{name: "empty stdin", input: "", wantErr: "stdin ended before the password line"},
		{name: "an empty line", input: "\nshutdown\n", wantErr: "the password line on stdin is empty"},
		{name: "a blank line", input: " \t\r\n", wantErr: "the password line on stdin is empty"},
		{name: "over-long", input: strings.Repeat("x", maxStdinPasswordBytes+1) + "\n", wantErr: "longer than 4096 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPasswordLine(bufio.NewReaderSize(strings.NewReader(tt.input), maxStdinPasswordBytes))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("readPasswordLine() = %q, %v; want an error with %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("readPasswordLine() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := readPasswordLine(bufio.NewReader(failingReader{})); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("a failing stdin: %v", err)
	}
}

// The password and a shutdown line written at once: the lifeline still gets the shutdown line
// the password reader buffered.
func TestThePasswordLineLeavesTheLifelineIntact(t *testing.T) {
	shared := bufio.NewReaderSize(strings.NewReader("s3cret\nshutdown\n"), maxStdinPasswordBytes)
	if got, err := readPasswordLine(shared); err != nil || got != "s3cret" {
		t.Fatalf("readPasswordLine() = %q, %v", got, err)
	}
	if got := watchLifeline(shared); got != "shutdown requested" {
		t.Fatalf("watchLifeline() = %q, want shutdown requested", got)
	}
}

func TestReportListen(t *testing.T) {
	report := captureLifelineReport(t)
	reportListen("127.0.0.1:18417", nil)
	reportListen("127.0.0.1:18418", errors.New("listen tcp 127.0.0.1:18418:\r\n  bind: address already in use"))
	want := "lifeline: listening on 127.0.0.1:18417\n" +
		"lifeline: cannot listen on 127.0.0.1:18418: listen tcp 127.0.0.1:18418: bind: address already in use\n"
	if got := report.String(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
}

func TestWatchLifeline(t *testing.T) {
	long := strings.Repeat("x", 10_000) + " shutdown"
	tests := []struct {
		name  string
		input io.Reader
		want  string
	}{
		{name: "EOF", input: strings.NewReader(""), want: "stdin closed"},
		{name: "shutdown line", input: strings.NewReader("shutdown\n"), want: "shutdown requested"},
		{name: "CRLF", input: strings.NewReader("shutdown\r\n"), want: "shutdown requested"},
		{name: "padded, without a newline before EOF", input: strings.NewReader("  shutdown "), want: "shutdown requested"},
		{name: "after other lines", input: strings.NewReader("\nhello\nshutdown\nignored\n"), want: "shutdown requested"},
		{name: "other lines, then EOF", input: strings.NewReader("hello\nshutdownx\nSHUTDOWN\n"), want: "stdin closed"},
		{name: "an over-long line never matches", input: strings.NewReader(long + "\n"), want: "stdin closed"},
		{name: "an over-long line, then shutdown", input: strings.NewReader(long + "\nshutdown\n"), want: "shutdown requested"},
		{name: "read error", input: failingReader{}, want: "stdin failed: broken pipe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchLifeline(tt.input); got != tt.want {
				t.Fatalf("watchLifeline() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWatchLifelineWaitsForTheSupervisor(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan string, 1)
	go func() { done <- watchLifeline(reader) }()

	// io.Pipe's Write returns once the watcher has read the line.
	if _, err := writer.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-done:
		t.Fatalf("returned %q on an unknown line", reason)
	default:
	}
	if _, err := writer.Write([]byte("shutdown\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-done:
		if reason != "shutdown requested" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown line did not end the watch")
	}
}

func TestDevyreWithLifelineEndsTheContextOnEOF(t *testing.T) {
	reader, writer := io.Pipe()
	ctx, _ := devyreWithLifeline(context.Background(), cliproxy.NewBuilder(), reader, "")
	if ctx.Err() != nil {
		t.Fatal("the context ended before the lifeline did")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("closing stdin did not end the context")
	}
}

// lifelineTestConfig writes and loads a sidecar-like config on port.
func lifelineTestConfig(t *testing.T, port int) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	raw := fmt.Sprintf("config-version: 8\nserver:\n  host: \"127.0.0.1\"\n  port: %d\n  commercial-mode: true\n"+
		"management:\n  allow-remote: false\n  disable-control-panel: true\n  disable-auto-update-panel: true\n"+
		"oauth:\n  auth-dir: %q\nobservability:\n  logs:\n    logging-to-file: false\n  usage:\n    usage-statistics-enabled: false\n"+
		"plugins:\n  enabled: false\n", port, filepath.Join(dir, "auths"))
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, configPath
}

// The whole server, in process: it reports its listener once it holds the port, /keep-alive
// answers the local password and a challenge with no idle limit configured, and the lifeline stops
// StartService gracefully on a shutdown line and on EOF.
func TestStartServiceStopsOnTheLifeline(t *testing.T) {
	for _, ending := range []string{"shutdown line", "EOF"} {
		t.Run(ending, func(t *testing.T) {
			port := freeLoopbackPort(t)
			cfg, configPath := lifelineTestConfig(t, port)
			report := captureLifelineReport(t)
			var err error

			reader, writer := io.Pipe()
			previous := lifelineInput
			lifelineInput = reader
			t.Cleanup(func() { lifelineInput = previous })

			const password = "lifeline-test-password"
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				StartService(cfg, configPath, password)
			}()

			keepAlive := func(secret string) int {
				request, errRequest := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/keep-alive", port), nil)
				if errRequest != nil {
					t.Fatal(errRequest)
				}
				request.Header.Set("Authorization", "Bearer "+secret)
				response, errDo := http.DefaultClient.Do(request)
				if errDo != nil {
					return 0
				}
				_ = response.Body.Close()
				return response.StatusCode
			}
			deadline := time.Now().Add(30 * time.Second)
			for keepAlive(password) != http.StatusOK {
				select {
				case <-stopped:
					t.Fatal("the server stopped before it served /keep-alive")
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the server did not serve /keep-alive")
				}
				time.Sleep(50 * time.Millisecond)
			}
			if got := keepAlive("wrong"); got != http.StatusUnauthorized {
				t.Fatalf("a wrong password: status %d, want 401", got)
			}
			// Printed before the first connection was accepted, so it is there by now.
			address := fmt.Sprintf("127.0.0.1:%d", port)
			if got := report.String(); got != LifelineListeningPrefix+address+"\n" {
				t.Fatalf("listening line = %q, want %q", got, LifelineListeningPrefix+address)
			}
			const challenge = "c2lkZWNhci1jaGFsbGVuZ2UtMDAx"
			request, errRequest := http.NewRequest(http.MethodGet, "http://"+address+"/keep-alive", nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			request.Header.Set(api.LifelineChallengeHeader, challenge)
			response, errDo := http.DefaultClient.Do(request)
			if errDo != nil {
				t.Fatal(errDo)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK || !strings.Contains(string(body), api.LifelineProof(password, address, challenge)) {
				t.Fatalf("challenge: status %d body %s, want the proof", response.StatusCode, body)
			}

			if ending == "EOF" {
				err = writer.Close()
			} else {
				_, err = writer.Write([]byte(LifelineShutdownLine + "\n"))
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-stopped:
			case <-time.After(30 * time.Second):
				t.Fatal("StartService did not return")
			}
			if keepAlive(password) != 0 {
				t.Fatal("the server still answers after the lifeline ended")
			}
		})
	}
}

// A port another program holds: the server says so on its report line and StartService returns,
// so a supervisor learns it without ever sending a request to that program.
func TestStartServiceReportsATakenPort(t *testing.T) {
	held, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port
	cfg, configPath := lifelineTestConfig(t, port)
	report := captureLifelineReport(t)

	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	previous := lifelineInput
	lifelineInput = reader
	t.Cleanup(func() { lifelineInput = previous })

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		StartService(cfg, configPath, "lifeline-test-password")
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("StartService kept running on a port it could not bind")
	}
	want := fmt.Sprintf("%s127.0.0.1:%d: ", LifelineCannotListenPrefix, port)
	if got := report.String(); !strings.HasPrefix(got, want) || strings.Count(got, "\n") != 1 {
		t.Fatalf("report = %q, want one line starting with %q", got, want)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
