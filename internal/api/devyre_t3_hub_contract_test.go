package api

// T3 Code's "CLIProxyAPI hub" (pingdotgg/t3code, apps/server/src/usage/cliproxyApi.ts)
// reads account quota through the deprecated /v0/management API. The tests in this
// file pin exactly what that client decodes, so an upstream sync that changes or
// removes those endpoints fails here first. If upstream deletes the v0 routes,
// restore them in internal/api/devyre_v0_shim.go (devyre/PLAN.md, SRV-2 and D8).
//
// What T3 decodes (Effect Schema; unknown keys are ignored, so extra fields are fine,
// but a required key with the wrong JSON type or an optional key sent as null fails
// the whole response):
//
//	GET  /v0/management/auth-files  -> {files: [{id: string, auth_index: string, provider: string,
//	                                    email?: string, disabled?: boolean,
//	                                    id_token?: {chatgpt_account_id?: string, chatgpt_plan_type?: string}}]}
//	POST /v0/management/api-call    -> {status_code: number, body: string}
//	POST /v0/management/reset-quota -> any 2xx JSON body
//
// Every call carries "Authorization: Bearer <plaintext management key>" and reaches
// the server through tailscale serve, so the hub is a remote management client.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	t3HubManagementKey = "t3-hub-contract-management-key"
	t3HubProxyPeer     = "172.17.0.1:41234" // Docker Desktop gateway in front of the container
	t3HubTailnetIP     = "100.64.0.10"      // tailnet client IP forwarded by tailscale serve
	t3HubCodexAccount  = "acct-t3hub-0001"

	t3HubClaudeEmail   = "claude-active@example.com"
	t3HubDisabledEmail = "claude-disabled@example.com"
	t3HubCodexEmail    = "codex-user@example.com"

	t3HubClaudeUsageBody = `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T23:00:00+00:00"},"seven_day":{"utilization":40,"resets_at":"2026-10-08T17:00:00+00:00"}}`
	t3HubCodexUsageBody  = `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":9,"reset_at":1791079200,"limit_window_seconds":18000},"secondary_window":{"used_percent":31,"reset_at":1791500400,"limit_window_seconds":604800}}}`
	t3HubCreditBody      = `{"redeem_request_id":"8f5bd4f1-62a4-5c4e-9a43-1f1f6c2b9d01","credit_id":"credit-t3hub-1"}`
)

type t3HubFixture struct {
	server  *Server
	manager *auth.Manager
}

// t3HubAccount is the part of an auth-files entry that T3 decodes.
type t3HubAccount struct {
	ID        string
	AuthIndex string
	Provider  string
	Email     string
	HasEmail  bool
	Disabled  bool
	IDToken   map[string]any
}

type t3HubAPIResponse struct {
	StatusCode int
	Header     map[string][]string
	Body       string
}

type t3HubUpstreamCall struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

type t3HubUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	calls []t3HubUpstreamCall
}

// t3HubStandardAuthFiles mirrors a pool with two Claude accounts (one disabled), one
// Codex account and one provider T3 ignores but must still decode.
func t3HubStandardAuthFiles(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"claude-" + t3HubClaudeEmail + ".json":   `{"type":"claude","email":"` + t3HubClaudeEmail + `","access_token":"t3hub-claude-access","refresh_token":"t3hub-claude-refresh"}`,
		"claude-" + t3HubDisabledEmail + ".json": `{"type":"claude","email":"` + t3HubDisabledEmail + `","access_token":"t3hub-claude-disabled-access","disabled":true}`,
		"codex-" + t3HubCodexEmail + "-pro.json": fmt.Sprintf(`{"type":"codex","email":%q,"access_token":"t3hub-codex-access","refresh_token":"t3hub-codex-refresh","account_id":%q,"id_token":%q}`,
			t3HubCodexEmail, t3HubCodexAccount, t3HubCodexIDToken(t)),
		"kimi-no-email.json": `{"type":"kimi","access_token":"t3hub-kimi-access"}`,
	}
}

// t3HubCodexIDToken builds an unsigned ChatGPT id_token carrying the claims the
// auth-files listing exposes under id_token.
func t3HubCodexIDToken(t *testing.T) string {
	t.Helper()
	claims, errMarshal := json.Marshal(map[string]any{
		"email": t3HubCodexEmail,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": t3HubCodexAccount,
			"chatgpt_plan_type":  "pro",
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal id_token claims: %v", errMarshal)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
}

// newT3HubFixture starts the real server stack the way production does: a config
// file whose plaintext secret-key is hashed on load, and auth JSON files loaded by
// the file token store.
func newT3HubFixture(t *testing.T, authFiles map[string]string) *t3HubFixture {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth dir: %v", errMkdir)
	}
	for name, body := range authFiles {
		if errWrite := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); errWrite != nil {
			t.Fatalf("write auth file %s: %v", name, errWrite)
		}
	}

	// Same management shape as devyre/deploy/config.template.yaml: remote access for
	// the tailnet and the Docker gateway trusted to forward the real client IP.
	configPath := filepath.Join(dir, "config.yaml")
	rawConfig := fmt.Sprintf(`config-version: 8
server:
  port: 8317
  trusted-proxies:
    - 172.16.0.0/12
management:
  allow-remote: true
  secret-key: %q
access:
  api-keys:
    - "t3-hub-contract-client-key"
oauth:
  auth-dir: %q
`, t3HubManagementKey, filepath.ToSlash(authDir))
	if errWrite := os.WriteFile(configPath, []byte(rawConfig), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	cfg, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	if !cfg.RemoteManagement.AllowRemote {
		t.Fatal("management.allow-remote did not load as true")
	}
	if secret := cfg.RemoteManagement.SecretKey; secret == "" || secret == t3HubManagementKey {
		t.Fatal("management.secret-key was not hashed on load; the hub must authenticate against the hash")
	}
	cfg.Debug = true // keep gin in test mode, like newTestServerWithConfig

	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := auth.NewManager(store, nil, nil)
	if errLoadAuths := manager.Load(context.Background()); errLoadAuths != nil {
		t.Fatalf("load auth files: %v", errLoadAuths)
	}

	server := NewServer(cfg, manager, sdkaccess.NewManager(), configPath)
	return &t3HubFixture{server: server, manager: manager}
}

// request sends a management call shaped like T3's hub client: a bearer key and,
// for POSTs, a JSON body, arriving from the tailnet through tailscale serve.
func (f *t3HubFixture) request(t *testing.T, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			t.Fatalf("marshal request body: %v", errMarshal)
		}
		payload = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, payload)
	req.RemoteAddr = t3HubProxyPeer
	req.Header.Set("X-Forwarded-For", t3HubTailnetIP)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.server.engine.ServeHTTP(rec, req)
	return rec
}

func (f *t3HubFixture) listAccounts(t *testing.T) []t3HubAccount {
	t.Helper()
	rec := f.request(t, http.MethodGet, "/v0/management/auth-files", t3HubManagementKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v0/management/auth-files status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	return decodeT3HubAuthFiles(t, rec.Body.Bytes())
}

// decodeT3HubAuthFiles applies T3's AuthFiles schema: required keys must be present
// with the declared JSON type, and optional keys, when present, must not be null.
func decodeT3HubAuthFiles(t *testing.T, raw []byte) []t3HubAccount {
	t.Helper()
	var payload map[string]any
	if errDecode := json.Unmarshal(raw, &payload); errDecode != nil {
		t.Fatalf("auth-files response is not a JSON object: %v; body = %s", errDecode, raw)
	}
	files, ok := payload["files"].([]any)
	if !ok {
		t.Fatalf("auth-files files = %#v, want a JSON array; body = %s", payload["files"], raw)
	}
	accounts := make([]t3HubAccount, 0, len(files))
	for i, item := range files {
		entry, okEntry := item.(map[string]any)
		if !okEntry {
			t.Fatalf("files[%d] = %#v, want an object", i, item)
		}
		account := t3HubAccount{
			ID:        t3HubRequireString(t, entry, i, "id"),
			AuthIndex: t3HubRequireString(t, entry, i, "auth_index"),
			Provider:  t3HubRequireString(t, entry, i, "provider"),
		}
		if account.ID == "" || account.AuthIndex == "" {
			t.Fatalf("files[%d] has an empty id or auth_index: %#v", i, entry)
		}
		if value, present := entry["email"]; present {
			email, okEmail := value.(string)
			if !okEmail {
				t.Fatalf("files[%d].email = %#v, want a string (T3 rejects null)", i, value)
			}
			account.Email, account.HasEmail = email, true
		}
		if value, present := entry["disabled"]; present {
			disabled, okDisabled := value.(bool)
			if !okDisabled {
				t.Fatalf("files[%d].disabled = %#v, want a boolean", i, value)
			}
			account.Disabled = disabled
		}
		if value, present := entry["id_token"]; present {
			claims, okClaims := value.(map[string]any)
			if !okClaims {
				t.Fatalf("files[%d].id_token = %#v, want an object", i, value)
			}
			for _, key := range []string{"chatgpt_account_id", "chatgpt_plan_type"} {
				if claim, presentClaim := claims[key]; presentClaim {
					if _, okClaim := claim.(string); !okClaim {
						t.Fatalf("files[%d].id_token.%s = %#v, want a string", i, key, claim)
					}
				}
			}
			account.IDToken = claims
		}
		accounts = append(accounts, account)
	}
	return accounts
}

func t3HubRequireString(t *testing.T, entry map[string]any, index int, key string) string {
	t.Helper()
	value, present := entry[key]
	if !present {
		t.Fatalf("files[%d] is missing %q: %#v", index, key, entry)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("files[%d].%s = %#v (%T), want a JSON string", index, key, value, value)
	}
	return text
}

func findT3HubAccount(t *testing.T, accounts []t3HubAccount, email string) t3HubAccount {
	t.Helper()
	for _, account := range accounts {
		if account.Email == email {
			return account
		}
	}
	t.Fatalf("no auth-files entry with email %q in %#v", email, accounts)
	return t3HubAccount{}
}

// t3HubAPICallBody builds the api-call payload exactly as T3's apiCall does. An
// empty data string means a GET; otherwise T3 POSTs the JSON-encoded data.
func t3HubAPICallBody(account t3HubAccount, url, data string) map[string]any {
	header := map[string]string{"Authorization": "Bearer $TOKEN$", "anthropic-beta": "oauth-2025-04-20"}
	if account.Provider == "codex" {
		header = map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"OpenAI-Beta":   "codex-1",
			"Originator":    "Codex Desktop",
		}
		if accountID, _ := account.IDToken["chatgpt_account_id"].(string); accountID != "" {
			header["Chatgpt-Account-Id"] = accountID
		}
	}
	body := map[string]any{"auth_index": account.AuthIndex, "method": http.MethodGet, "url": url, "header": header}
	if data != "" {
		body["method"] = http.MethodPost
		body["data"] = data
	}
	return body
}

// decodeT3HubAPIResponse applies T3's ApiResponse schema (status_code number, body
// string) plus the header map the panel reads from the same response.
func decodeT3HubAPIResponse(t *testing.T, raw []byte) t3HubAPIResponse {
	t.Helper()
	var payload map[string]any
	if errDecode := json.Unmarshal(raw, &payload); errDecode != nil {
		t.Fatalf("api-call response is not a JSON object: %v; body = %s", errDecode, raw)
	}
	statusCode, ok := payload["status_code"].(float64)
	if !ok {
		t.Fatalf("api-call status_code = %#v, want a JSON number", payload["status_code"])
	}
	body, ok := payload["body"].(string)
	if !ok {
		t.Fatalf("api-call body = %#v, want a JSON string", payload["body"])
	}
	headerObject, ok := payload["header"].(map[string]any)
	if !ok {
		t.Fatalf("api-call header = %#v, want a JSON object", payload["header"])
	}
	header := make(map[string][]string, len(headerObject))
	for name, rawValues := range headerObject {
		values, okValues := rawValues.([]any)
		if !okValues {
			t.Fatalf("api-call header[%q] = %#v, want an array of strings", name, rawValues)
		}
		for _, rawValue := range values {
			value, okValue := rawValue.(string)
			if !okValue {
				t.Fatalf("api-call header[%q] contains %#v, want strings", name, rawValue)
			}
			header[name] = append(header[name], value)
		}
	}
	return t3HubAPIResponse{StatusCode: int(statusCode), Header: header, Body: body}
}

// newT3HubUpstream stands in for api.anthropic.com and chatgpt.com and records
// what the server relays.
func newT3HubUpstream(t *testing.T) *t3HubUpstream {
	t.Helper()
	upstream := &t3HubUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.calls = append(upstream.calls, t3HubUpstreamCall{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: string(body)})
		upstream.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-T3hub-Upstream", "contract")
		switch r.URL.Path {
		case "/api/oauth/usage":
			_, _ = io.WriteString(w, t3HubClaudeUsageBody)
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, t3HubCodexUsageBody)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			_, _ = io.WriteString(w, `{"code":"reset"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *t3HubUpstream) recorded() []t3HubUpstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]t3HubUpstreamCall(nil), u.calls...)
}

func TestT3Hub_AuthFilesMatchHubSchema(t *testing.T) {
	f := newT3HubFixture(t, t3HubStandardAuthFiles(t))
	accounts := f.listAccounts(t)
	if len(accounts) != 4 {
		t.Fatalf("auth-files returned %d entries, want 4: %#v", len(accounts), accounts)
	}

	seenIndexes := make(map[string]string, len(accounts))
	for _, account := range accounts {
		if other, duplicate := seenIndexes[account.AuthIndex]; duplicate {
			t.Fatalf("auth_index %q is shared by %q and %q", account.AuthIndex, other, account.ID)
		}
		seenIndexes[account.AuthIndex] = account.ID
	}

	claude := findT3HubAccount(t, accounts, t3HubClaudeEmail)
	if claude.Provider != "claude" || claude.Disabled {
		t.Fatalf("active Claude entry = %#v, want provider claude and disabled false", claude)
	}
	if disabled := findT3HubAccount(t, accounts, t3HubDisabledEmail); disabled.Provider != "claude" || !disabled.Disabled {
		t.Fatalf("disabled Claude entry = %#v, want provider claude and disabled true", disabled)
	}

	codex := findT3HubAccount(t, accounts, t3HubCodexEmail)
	if codex.Provider != "codex" || codex.Disabled {
		t.Fatalf("Codex entry = %#v, want provider codex and disabled false", codex)
	}
	if codex.IDToken == nil {
		t.Fatalf("Codex entry has no id_token object: %#v", codex)
	}
	if got := codex.IDToken["chatgpt_account_id"]; got != t3HubCodexAccount {
		t.Fatalf("Codex id_token.chatgpt_account_id = %#v, want %q (T3 sends it as Chatgpt-Account-Id)", got, t3HubCodexAccount)
	}
	// T3 reads id_token.chatgpt_plan_type only as a fallback plan label when
	// /wham/usage omits plan_type. The server currently reports the plan as
	// id_token.plan_type, which T3 ignores; accept either spelling so this pins that
	// the plan reaches the listing without failing if upstream adopts T3's key.
	planType, _ := codex.IDToken["chatgpt_plan_type"].(string)
	if planType == "" {
		planType, _ = codex.IDToken["plan_type"].(string)
	}
	if planType != "pro" {
		t.Fatalf("Codex id_token plan type = %q, want %q; id_token = %#v", planType, "pro", codex.IDToken)
	}

	for _, account := range accounts {
		if strings.Contains(account.ID, "kimi-no-email") && account.HasEmail {
			t.Fatalf("entry without a known email sent email %q; it must omit the key", account.Email)
		}
	}

	// The hub reads only enabled claude and codex accounts.
	var read []string
	for _, account := range accounts {
		if !account.Disabled && (account.Provider == "claude" || account.Provider == "codex") {
			read = append(read, account.Email)
		}
	}
	sort.Strings(read)
	if want := []string{t3HubClaudeEmail, t3HubCodexEmail}; !reflect.DeepEqual(read, want) {
		t.Fatalf("accounts T3 would read = %v, want %v", read, want)
	}
}

func TestT3Hub_AuthFilesWithoutAccountsIsEmptyArray(t *testing.T) {
	f := newT3HubFixture(t, nil)
	rec := f.request(t, http.MethodGet, "/v0/management/auth-files", t3HubManagementKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var payload map[string]json.RawMessage
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode: %v; body = %s", errDecode, rec.Body.String())
	}
	if got := strings.TrimSpace(string(payload["files"])); got != "[]" {
		t.Fatalf("files = %s, want [] (T3 rejects null)", got)
	}
}

func TestT3Hub_APICallRelaysWithSubstitutedToken(t *testing.T) {
	f := newT3HubFixture(t, t3HubStandardAuthFiles(t))
	upstream := newT3HubUpstream(t)
	accounts := f.listAccounts(t)
	claude := findT3HubAccount(t, accounts, t3HubClaudeEmail)
	codex := findT3HubAccount(t, accounts, t3HubCodexEmail)

	codexHeaders := map[string]string{
		"Content-Type":       "application/json",
		"Openai-Beta":        "codex-1",
		"Originator":         "Codex Desktop",
		"Chatgpt-Account-Id": t3HubCodexAccount,
	}
	cases := []struct {
		name        string
		account     t3HubAccount
		path        string
		data        string
		wantMethod  string
		wantToken   string
		wantBody    string
		wantHeaders map[string]string
	}{
		{
			name:        "claude usage",
			account:     claude,
			path:        "/api/oauth/usage",
			wantMethod:  http.MethodGet,
			wantToken:   "t3hub-claude-access",
			wantBody:    t3HubClaudeUsageBody,
			wantHeaders: map[string]string{"Anthropic-Beta": "oauth-2025-04-20"},
		},
		{
			name:        "codex usage",
			account:     codex,
			path:        "/backend-api/wham/usage",
			wantMethod:  http.MethodGet,
			wantToken:   "t3hub-codex-access",
			wantBody:    t3HubCodexUsageBody,
			wantHeaders: codexHeaders,
		},
		{
			name:        "codex credit redeem",
			account:     codex,
			path:        "/backend-api/wham/rate-limit-reset-credits/consume",
			data:        t3HubCreditBody,
			wantMethod:  http.MethodPost,
			wantToken:   "t3hub-codex-access",
			wantBody:    `{"code":"reset"}`,
			wantHeaders: codexHeaders,
		},
	}
	routes := []struct{ name, path string }{
		{name: "v0", path: "/v0/management/api-call"},          // T3 hub
		{name: "v8", path: "/v8/management/requests/api-call"}, // panel
	}
	for _, route := range routes {
		for _, tc := range cases {
			t.Run(route.name+" "+tc.name, func(t *testing.T) {
				before := len(upstream.recorded())
				rec := f.request(t, http.MethodPost, route.path, t3HubManagementKey, t3HubAPICallBody(tc.account, upstream.URL+tc.path, tc.data))
				if rec.Code != http.StatusOK {
					t.Fatalf("POST %s status = %d, want 200; body = %s", route.path, rec.Code, rec.Body.String())
				}
				response := decodeT3HubAPIResponse(t, rec.Body.Bytes())
				if response.StatusCode != http.StatusOK {
					t.Fatalf("status_code = %d, want 200", response.StatusCode)
				}
				if response.Body != tc.wantBody {
					t.Fatalf("body = %q, want the upstream body %q", response.Body, tc.wantBody)
				}
				if got := response.Header["X-T3hub-Upstream"]; len(got) != 1 || got[0] != "contract" {
					t.Fatalf("header[X-T3hub-Upstream] = %v, want the upstream response headers", got)
				}

				calls := upstream.recorded()
				if len(calls) != before+1 {
					t.Fatalf("upstream received %d calls, want exactly 1", len(calls)-before)
				}
				call := calls[len(calls)-1]
				if call.Method != tc.wantMethod || call.Path != tc.path {
					t.Fatalf("upstream saw %s %s, want %s %s", call.Method, call.Path, tc.wantMethod, tc.path)
				}
				if got, want := call.Header.Get("Authorization"), "Bearer "+tc.wantToken; got != want {
					t.Fatalf("upstream Authorization = %q, want %q ($TOKEN$ must be substituted)", got, want)
				}
				for name, want := range tc.wantHeaders {
					if got := call.Header.Get(name); got != want {
						t.Fatalf("upstream header %s = %q, want %q", name, got, want)
					}
				}
				if call.Body != tc.data {
					t.Fatalf("upstream body = %q, want %q", call.Body, tc.data)
				}
			})
		}
	}
}

func TestT3Hub_ResetQuotaClearsCooldownAndReturnsJSON(t *testing.T) {
	f := newT3HubFixture(t, t3HubStandardAuthFiles(t))
	codex := findT3HubAccount(t, f.listAccounts(t), t3HubCodexEmail)

	// Put the credential into a quota cooldown, as an upstream 429 would. T3 calls
	// reset-quota after redeeming a Codex reset credit so routing resumes at once.
	current, ok := f.manager.GetByID(codex.ID)
	if !ok || current == nil {
		t.Fatalf("auth %q is not registered", codex.ID)
	}
	cooling := current.Clone()
	next := time.Now().Add(time.Hour)
	cooling.Status = auth.StatusError
	cooling.StatusMessage = "quota exhausted"
	cooling.Unavailable = true
	cooling.NextRetryAfter = next
	cooling.Quota = auth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 1}
	if _, errUpdate := f.manager.Update(context.Background(), cooling); errUpdate != nil {
		t.Fatalf("set cooldown: %v", errUpdate)
	}
	if before, _ := f.manager.GetByID(codex.ID); before == nil || !before.Unavailable || !before.Quota.Exceeded {
		t.Fatalf("precondition: credential is not cooling down: %#v", before)
	}

	rec := f.request(t, http.MethodPost, "/v0/management/reset-quota", t3HubManagementKey, map[string]string{"auth_index": codex.AuthIndex})
	if rec.Code < 200 || rec.Code > 299 {
		t.Fatalf("POST /v0/management/reset-quota status = %d, want 2xx; body = %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("reset-quota body is not JSON (T3 parses it): %v; body = %s", errDecode, rec.Body.String())
	}

	after, ok := f.manager.GetByID(codex.ID)
	if !ok || after == nil {
		t.Fatalf("auth %q disappeared after reset", codex.ID)
	}
	if after.Unavailable || after.Quota.Exceeded || !after.NextRetryAfter.IsZero() {
		t.Fatalf("cooldown not cleared: unavailable=%v exceeded=%v next_retry_after=%v", after.Unavailable, after.Quota.Exceeded, after.NextRetryAfter)
	}
}

func TestT3Hub_WrongKeyIsRejectedWithoutEarlyBan(t *testing.T) {
	f := newT3HubFixture(t, t3HubStandardAuthFiles(t))
	upstream := newT3HubUpstream(t)
	claude := findT3HubAccount(t, f.listAccounts(t), t3HubClaudeEmail)

	// A mistyped hub key must be rejected on every endpoint T3 calls, and four
	// failures from one tailnet client must not ban it (the ban starts at five).
	wrongKey := "wrong-" + t3HubManagementKey
	attempts := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v0/management/auth-files", nil},
		{http.MethodPost, "/v0/management/api-call", t3HubAPICallBody(claude, upstream.URL+"/api/oauth/usage", "")},
		{http.MethodPost, "/v0/management/reset-quota", map[string]string{"auth_index": claude.AuthIndex}},
		{http.MethodPost, "/v8/management/requests/api-call", t3HubAPICallBody(claude, upstream.URL+"/api/oauth/usage", "")},
	}
	for _, attempt := range attempts {
		rec := f.request(t, attempt.method, attempt.path, wrongKey, attempt.body)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a wrong key: status = %d, want 401 or 403; body = %s", attempt.method, attempt.path, rec.Code, rec.Body.String())
		}
	}
	if calls := upstream.recorded(); len(calls) != 0 {
		t.Fatalf("a rejected api-call reached the upstream %d times", len(calls))
	}

	rec := f.request(t, http.MethodGet, "/v0/management/auth-files", t3HubManagementKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct key after %d failures: status = %d, want 200 (client banned too early?); body = %s", len(attempts), rec.Code, rec.Body.String())
	}
}
