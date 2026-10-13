package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

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

// The whole server, in process: /keep-alive answers the local password with no idle limit
// configured, and the lifeline stops StartService gracefully on a shutdown line and on EOF.
func TestStartServiceStopsOnTheLifeline(t *testing.T) {
	for _, ending := range []string{"shutdown line", "EOF"} {
		t.Run(ending, func(t *testing.T) {
			dir := t.TempDir()
			port := freeLoopbackPort(t)
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
