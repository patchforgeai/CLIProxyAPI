package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// A conversation that began before midnight keeps its date through a restart
// after midnight, with its continuity in the file: its first message upstream,
// the start of its cached prefix, stays byte for byte as it was. Without the
// file the restart pins the new date.
func TestDevyreClaudeCloakedDateSurvivesARestart(t *testing.T) {
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)
	var mockTime time.Time
	claudeCodeCurrentTimeFunc = func(*config.Config, *cliproxyauth.Auth) time.Time { return mockTime }
	t.Cleanup(func() { claudeCodeCurrentTimeFunc = nil })

	var bodies [][]byte
	transport := datePinRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		bodies = append(bodies, body)
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		header.Set("request-id", fmt.Sprintf("req_restart_%d", len(bodies)))
		response := fmt.Sprintf(`{"id":"msg_restart_%d","type":"message","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":10,"output_tokens":2}}`, len(bodies))
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	auth := &cliproxyauth.Auth{
		ID:         "devyre-restart-oauth",
		Attributes: map[string]string{"api_key": "sk-ant-oat-devyre-restart", "cloak_mode": "always", "timezone": "UTC"},
		Metadata: map[string]any{
			"account_uuid":                        "11111111-2222-4333-8444-555555555555",
			claudeauth.ClaudeDeviceIDsMetadataKey: []string{"00000000000000000000000000000000000000000000000000000000000000c1"},
		},
	}
	exec := NewClaudeExecutor(&config.Config{})
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"X-Session-ID": []string{"devyre-restart-session"}}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	send := func(stage string) []byte {
		t.Helper()
		payload := `{"model":"claude-opus-5","max_tokens":100,"messages":[{"role":"user","content":"Say OK."}]}`
		if _, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)}, options); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		return bodies[len(bodies)-1]
	}

	path := filepath.Join(t.TempDir(), helps.ClaudeContinuityFileName)
	persistence := helps.NewClaudeContinuityPersistence(path)
	persistence.Attach()
	mockTime = time.Date(2026, 10, 19, 23, 50, 0, 0, time.UTC)
	before := send("before midnight")
	persistence.Close() // the router stops

	// The router starts again after midnight, in a new process.
	helps.ResetClaudeDiagnosticsForTest()
	restarted := helps.NewClaudeContinuityPersistence(path)
	if restored := restarted.Load(); restored == 0 {
		t.Fatal("the restart restored no conversation")
	}
	restarted.Attach()
	mockTime = time.Date(2026, 10, 20, 0, 20, 0, 0, time.UTC)
	after := send("after the restart")
	restarted.Close()
	if got := extractClaudeCloakDate(t, after); got != "2026-10-19" {
		t.Fatalf("after the restart the conversation carries %s, want its first day 2026-10-19", got)
	}
	if first, again := gjson.GetBytes(before, "messages.0").Raw, gjson.GetBytes(after, "messages.0").Raw; first != again {
		t.Fatalf("the restart changed the first message:\nbefore: %s\nafter:  %s", first, again)
	}

	// Without the file, the same restart pins the new date.
	helps.ResetClaudeDiagnosticsForTest()
	if got := extractClaudeCloakDate(t, send("a restart without the file")); got != "2026-10-20" {
		t.Fatalf("a restart without the file carries %s; the test no longer shows what the file keeps", got)
	}
}
