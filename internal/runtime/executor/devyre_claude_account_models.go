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

// devyreClaudeModelsLimit bounds one page of the model list read (Anthropic's
// full list is a few tens of KB).
const devyreClaudeModelsLimit = 4 << 20

// DevyreFetchAccountModels asks Anthropic which models the credential's account
// may use: one page of GET <base>/v1/models?limit=1000 (after afterID when
// set), with the credential's own token, through the credential's proxy and TLS
// client. An OAuth credential declares the OAuth beta, as every OAuth request
// to the API does. It returns the body of a 200 answer
// (internal/claudeaccountmodels parses it). The caller bounds it with ctx.
func (e *ClaudeExecutor) DevyreFetchAccountModels(ctx context.Context, auth *cliproxyauth.Auth, afterID string) ([]byte, error) {
	if auth == nil {
		return nil, fmt.Errorf("claude account models: no credential")
	}
	apiKey, baseURL := claudeCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("claude account models: the credential has no token")
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	query := url.Values{"limit": {"1000"}}
	if afterID != "" {
		query.Set("after_id", afterID)
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v1/models?" + query.Encode()
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("claude account models: %w", errReq)
	}
	// The credential's token, as a key or a bearer (PrepareRequest).
	if errPrepare := e.PrepareRequest(req, auth); errPrepare != nil {
		return nil, fmt.Errorf("claude account models: %w", errPrepare)
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Accept", "application/json")
	if auth.AuthKind() == cliproxyauth.AuthKindOAuth {
		req.Header.Set("Anthropic-Beta", claudeOAuthBeta)
	}
	resp, errDo := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("claude account models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, devyreClaudeModelsLimit+1))
	if errRead != nil {
		return nil, fmt.Errorf("claude account models: reading the list: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it can carry account details.
		return nil, fmt.Errorf("claude account models: the list answered %d", resp.StatusCode)
	}
	if len(body) > devyreClaudeModelsLimit {
		return nil, fmt.Errorf("claude account models: the list is larger than %d bytes", devyreClaudeModelsLimit)
	}
	return body, nil
}
