package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyreCodexCatalogLimit bounds the catalog body read (OpenAI's is ~100 KB).
const devyreCodexCatalogLimit = 8 << 20

// DevyreFetchAccountModels is the HTTP executor's lookup: the auto executor is
// the one the service registers for "codex".
func (e *CodexAutoExecutor) DevyreFetchAccountModels(ctx context.Context, auth *cliproxyauth.Auth, clientVersion string) ([]byte, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex account models: no Codex executor")
	}
	return e.httpExec.DevyreFetchAccountModels(ctx, auth, clientVersion)
}

// DevyreFetchAccountModels asks OpenAI which models the credential's account
// may use with a Codex client of clientVersion: GET <base>/models?client_version=
// as Codex itself asks, with the credential's token and the identity headers
// every Codex upstream request carries, through the credential's proxy. It
// returns the body of a 200 answer (internal/codexaccountmodels parses it).
// The caller bounds it with ctx.
func (e *CodexExecutor) DevyreFetchAccountModels(ctx context.Context, auth *cliproxyauth.Auth, clientVersion string) ([]byte, error) {
	if auth == nil {
		return nil, fmt.Errorf("codex account models: no credential")
	}
	token, baseURL := codexCreds(auth)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("codex account models: the credential has no access token")
	}
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/models?client_version=" + url.QueryEscape(clientVersion)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("codex account models: %w", errReq)
	}
	// An empty header set: nothing of a downstream request is forwarded.
	applyCodexHeaders(req, auth, token, false, e.cfg, http.Header{})
	req.Header.Del("Content-Type")
	// The client every Codex upstream request uses (HttpRequest): the
	// credential's proxy and TLS fingerprint.
	resp, errDo := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codex account models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, devyreCodexCatalogLimit+1))
	if errRead != nil {
		return nil, fmt.Errorf("codex account models: reading the catalog: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it can carry account details.
		return nil, fmt.Errorf("codex account models: the catalog answered %d", resp.StatusCode)
	}
	if len(body) > devyreCodexCatalogLimit {
		return nil, fmt.Errorf("codex account models: the catalog is larger than %d bytes", devyreCodexCatalogLimit)
	}
	return body, nil
}
