package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// devyreAntigravityStandIn is Google's Code Assist API: it records the body of
// every generate request and answers it.
type devyreAntigravityStandIn struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (s *devyreAntigravityStandIn) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream body: %v", errRead)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		switch r.URL.Path {
		case "/v1internal:streamGenerateContent":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n")
		case "/v1internal:generateContent":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func (s *devyreAntigravityStandIn) last() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

// devyreAntigravityRoutes registers what an account answering the fixture
// serves, and publishes its routes, as the registration does
// (sdk/cliproxy/devyre_antigravity_account_models.go).
func devyreAntigravityRoutes(t *testing.T, authID, fixture string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "antigravityaccountmodels", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	answer, err := antigravityaccountmodels.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	infos := antigravityaccountmodels.ModelInfos(answer)
	byID := make(map[string]*registry.ModelInfo, len(infos))
	for _, info := range infos {
		byID[strings.ToLower(info.ID)] = info
	}
	routes := antigravityaccountmodels.Routes(answer, answer.ServedIDs())
	for _, route := range routes {
		infos = append(infos, antigravityaccountmodels.RouteInfo(route, byID))
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "antigravity", infos)
	store := antigravityaccountmodels.Default()
	store.SetRoutes(authID, routes)
	t.Cleanup(func() {
		store.SetRoutes(authID, nil)
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})
}

func devyreAntigravityAuth(id, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": baseURL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

// Through the executor against a stand-in for Google's Code Assist API: a
// Gemini API name reaches Google as the id the account serves it under, for
// the level the request names, and the request's thinking config is kept.
func TestDevyreAntigravityAccountModel_UpstreamGetsTheVariant(t *testing.T) {
	standIn := &devyreAntigravityStandIn{}
	server := httptest.NewServer(standIn.handler(t))
	defer server.Close()

	auth := devyreAntigravityAuth("ag-devyre-variant.json", server.URL)
	devyreAntigravityRoutes(t, auth.ID, "fetch_available_models_full.json")
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})

	gemini := func(level string) []byte {
		if level == "" {
			return []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		}
		return []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}}}`)
	}
	for _, tc := range []struct {
		name, model, level, want string
		stream                   bool
		auth                     *cliproxyauth.Auth
	}{
		{name: "a family at its level", model: "gemini-4-neon", level: "LOW", want: "gemini-4-neon-low"},
		{name: "a family without a level", model: "gemini-4-neon", want: "gemini-4-neon-xhigh", stream: true},
		{name: "a family at its level, streamed", model: "gemini-4-carbon", level: "MEDIUM", want: "gemini-4-carbon-medium", stream: true},
		{name: "a suffix names the level", model: "gemini-4-neon(medium)", level: "LOW", want: "gemini-4-neon-medium"},
		{name: "a retired model", model: "gemini-3.7-flash", level: "LOW", want: "gemini-3.8-flash-low"},
		{name: "a name without its level suffix", model: "gemini-3.1-pro", want: "gemini-3.1-pro-low"},
		{name: "an id passes as it is", model: "gemini-4-neon-high", level: "LOW", want: "gemini-4-neon-high"},
		{name: "an account without routes", model: "gemini-4-neon", level: "LOW", want: "gemini-4-neon", auth: devyreAntigravityAuth("ag-devyre-no-routes.json", server.URL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := auth
			if tc.auth != nil {
				target = tc.auth
			}
			payload := gemini(tc.level)
			req := cliproxyexecutor.Request{Model: tc.model, Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, OriginalRequest: payload, Stream: tc.stream}
			if tc.stream {
				result, err := exec.ExecuteStream(context.Background(), target, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("chunk: %v", chunk.Err)
					}
				}
			} else if _, err := exec.Execute(context.Background(), target, req, opts); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			body := standIn.last()
			if got := gjson.GetBytes(body, "model").String(); got != tc.want {
				t.Fatalf("upstream model = %q, want %q; body %s", got, tc.want, body)
			}
			// The thinking config goes on as the request had it (a suffix wins over
			// the body, as it does without this).
			wantLevel := tc.level
			if suffix := thinking.ParseSuffix(tc.model); suffix.HasSuffix {
				wantLevel = suffix.RawSuffix
			}
			if got := gjson.GetBytes(body, "request.generationConfig.thinkingConfig.thinkingLevel").String(); !strings.EqualFold(got, wantLevel) {
				t.Fatalf("upstream thinking level = %q, want %q; body %s", got, wantLevel, body)
			}
		})
	}
}
