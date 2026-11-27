// Package claude provides OAuth2 authentication functionality for Anthropic's Claude API.
// This package implements the complete OAuth2 flow with PKCE (Proof Key for Code Exchange)
// for secure authentication with Claude API, including token exchange, refresh, and storage.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// OAuth configuration constants for Claude/Anthropic
const (
	AuthURL = "https://claude.ai/oauth/authorize"
	// TokenURL is the authorization-code exchange endpoint. Claude Code 2.1.220
	// posts the code exchange to platform.claude.com, not api.anthropic.com.
	TokenURL        = "https://platform.claude.com/v1/oauth/token"
	RefreshTokenURL = "https://platform.claude.com/v1/oauth/token"
	ProfileURL      = "https://api.anthropic.com/api/oauth/profile"
	// RolesURL is the claude_cli role endpoint the native client queries right
	// after a successful token exchange, alongside the profile lookup.
	RolesURL         = "https://api.anthropic.com/api/oauth/claude_cli/roles"
	ClientID         = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	RedirectURI      = "http://localhost:54545/callback"
	ClaudeOAuthScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	claudeRefreshHandshakeTimeout = 10 * time.Second
)

// tokenResponse represents the response structure from Anthropic's OAuth token endpoint.
// It contains access token, refresh token, and associated user/organization information.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

// authorizationCodeExchangeRequest is the authorization-code exchange body.
// Field order is significant: it mirrors the key order observed in native
// Claude Code 2.1.220 traffic to platform.claude.com/v1/oauth/token.
type authorizationCodeExchangeRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	ClientID     string `json:"client_id"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
}

// OAuthProfile is the account identity returned by Anthropic's OAuth profile endpoint.
type OAuthProfile struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// ClaudeAuth handles Anthropic OAuth2 authentication flow.
// It provides methods for generating authorization URLs, exchanging codes for tokens,
// and refreshing expired tokens using PKCE for enhanced security.
type ClaudeAuth struct {
	httpClient *http.Client
}

// NewClaudeAuth creates a new Anthropic authentication service.
// It initializes the HTTP client with a custom TLS transport that uses Firefox
// fingerprint to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
//
// Parameters:
//   - cfg: The application configuration containing proxy settings
//
// Returns:
//   - *ClaudeAuth: A new Claude authentication service instance
func NewClaudeAuth(cfg *config.Config) *ClaudeAuth {
	return NewClaudeAuthWithProxyURL(cfg, "")
}

// NewClaudeAuthWithProxyURL creates a new Anthropic authentication service with a proxy override.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewClaudeAuthWithProxyURL(cfg *config.Config, proxyURL string) *ClaudeAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg *config.SDKConfig
	if cfg != nil {
		sdkCfgCopy := cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
		sdkCfgCopy.ProxyURL = effectiveProxyURL
		sdkCfg = &sdkCfgCopy
	} else if effectiveProxyURL != "" {
		sdkCfgCopy := config.SDKConfig{ProxyURL: effectiveProxyURL}
		sdkCfg = &sdkCfgCopy
	}

	// Use custom HTTP client with Firefox TLS fingerprint to bypass
	// Cloudflare's bot detection on Anthropic domains.
	return &ClaudeAuth{
		httpClient: NewAnthropicHttpClient(sdkCfg),
	}
}

func applyClaudeOAuthAxiosHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "axios/1.15.2")
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	req.Header.Set("Connection", "close")
	req.Close = true
}

// fetchOAuthControlPlaneJSON issues an Axios-shaped OAuth control-plane GET and
// returns the decoded response body. label names the endpoint in error text.
func (o *ClaudeAuth) fetchOAuthControlPlaneJSON(ctx context.Context, endpoint, accessToken, label string) ([]byte, error) {
	if o == nil || o.httpClient == nil {
		return nil, fmt.Errorf("fetch Claude OAuth %s: HTTP client is nil", label)
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("fetch Claude OAuth %s: access token is empty", label)
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create Claude OAuth %s request: %w", label, errRequest)
	}
	applyClaudeOAuthAxiosHeaders(req)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Cache-Control", "no-cache")

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch Claude OAuth %s: %w", label, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close Claude OAuth %s response body: %v", label, errClose)
		}
	}()
	body, errRead := readClaudeOAuthResponseBody(resp)
	if errRead != nil {
		return nil, fmt.Errorf("read Claude OAuth %s response: %w", label, errRead)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch Claude OAuth %s failed with status %d", label, resp.StatusCode)
	}
	return body, nil
}

// FetchOAuthProfile retrieves the account identity associated with an OAuth access token.
func (o *ClaudeAuth) FetchOAuthProfile(ctx context.Context, accessToken string) (*OAuthProfile, error) {
	body, errFetch := o.fetchOAuthControlPlaneJSON(ctx, ProfileURL, accessToken, "profile")
	if errFetch != nil {
		return nil, errFetch
	}
	var profile OAuthProfile
	if errUnmarshal := json.Unmarshal(body, &profile); errUnmarshal != nil {
		return nil, fmt.Errorf("parse Claude OAuth profile response: %w", errUnmarshal)
	}
	if strings.TrimSpace(profile.Account.UUID) == "" {
		return nil, fmt.Errorf("fetch Claude OAuth profile: response account UUID is empty")
	}
	return &profile, nil
}

// FetchOAuthRoles performs the claude_cli roles lookup the native client issues
// alongside the profile query after a token exchange. Only the request shape is
// covered by captured evidence, so the payload stays opaque and is returned raw
// instead of being decoded into a guessed structure.
func (o *ClaudeAuth) FetchOAuthRoles(ctx context.Context, accessToken string) (json.RawMessage, error) {
	body, errFetch := o.fetchOAuthControlPlaneJSON(ctx, RolesURL, accessToken, "claude_cli roles")
	if errFetch != nil {
		return nil, errFetch
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("parse Claude OAuth claude_cli roles response: body is not valid JSON")
	}
	return json.RawMessage(body), nil
}

// inspectOAuthAccount replays the login companion control-plane calls the native
// client makes within roughly 500ms of a successful token exchange: the account
// profile lookup followed by the claude_cli roles lookup. Both are advisory, so
// failures are logged and never fail the surrounding login.
func (o *ClaudeAuth) inspectOAuthAccount(ctx context.Context, accessToken string) *OAuthProfile {
	profile, errProfile := o.FetchOAuthProfile(ctx, accessToken)
	if errProfile != nil {
		log.Warnf("fetch Claude OAuth profile after token exchange: %v", errProfile)
		profile = nil
	}
	if _, errRoles := o.FetchOAuthRoles(ctx, accessToken); errRoles != nil {
		log.Warnf("fetch Claude OAuth claude_cli roles after token exchange: %v", errRoles)
	}
	return profile
}

// GenerateAuthURL creates the OAuth authorization URL with PKCE.
// This method generates a secure authorization URL including PKCE challenge codes
// for the OAuth2 flow with Anthropic's API.
//
// Parameters:
//   - state: A random state parameter for CSRF protection
//   - pkceCodes: The PKCE codes for secure code exchange
//
// Returns:
//   - string: The complete authorization URL
//   - string: The state parameter for verification
//   - error: An error if PKCE codes are missing or URL generation fails
func (o *ClaudeAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes) (string, string, error) {
	// devyre: GenerateAuthURLWithRedirect (devyre_redirect.go) passes another loopback redirect.
	return o.generateAuthURL(state, pkceCodes, RedirectURI)
}

func (o *ClaudeAuth) generateAuthURL(state string, pkceCodes *PKCECodes, redirectURI string) (string, string, error) {
	if pkceCodes == nil {
		return "", "", fmt.Errorf("PKCE codes are required")
	}

	params := url.Values{
		"code":                  {"true"},
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {ClaudeOAuthScope},
		"code_challenge":        {pkceCodes.CodeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}

	authURL := fmt.Sprintf("%s?%s", AuthURL, params.Encode())
	return authURL, state, nil
}

// parseCodeAndState extracts the authorization code and state from the callback response.
// It handles the parsing of the code parameter which may contain additional fragments.
//
// Parameters:
//   - code: The raw code parameter from the OAuth callback
//
// Returns:
//   - parsedCode: The extracted authorization code
//   - parsedState: The extracted state parameter if present
func (c *ClaudeAuth) parseCodeAndState(code string) (parsedCode, parsedState string) {
	splits := strings.Split(code, "#")
	parsedCode = splits[0]
	if len(splits) > 1 {
		parsedState = splits[1]
	}
	return
}

// ExchangeCodeForTokens exchanges authorization code for access tokens.
// This method implements the OAuth2 token exchange flow using PKCE for security.
// It sends the authorization code along with PKCE verifier to get access and refresh tokens.
//
// Parameters:
//   - ctx: The context for the request
//   - code: The authorization code received from OAuth callback
//   - state: The state parameter for verification
//   - pkceCodes: The PKCE codes for secure verification
//
// Returns:
//   - *ClaudeAuthBundle: The complete authentication bundle with tokens
//   - error: An error if token exchange fails
func (o *ClaudeAuth) ExchangeCodeForTokens(ctx context.Context, code, state string, pkceCodes *PKCECodes) (*ClaudeAuthBundle, error) {
	// devyre: ExchangeCodeForTokensWithRedirect (devyre_redirect.go) passes the sign-in's redirect.
	return o.exchangeCodeForTokens(ctx, code, state, pkceCodes, RedirectURI)
}

func (o *ClaudeAuth) exchangeCodeForTokens(ctx context.Context, code, state string, pkceCodes *PKCECodes, redirectURI string) (*ClaudeAuthBundle, error) {
	if pkceCodes == nil {
		return nil, fmt.Errorf("PKCE codes are required for token exchange")
	}
	newCode, newState := o.parseCodeAndState(code)

	// Prepare token exchange request. The struct field order reproduces the key
	// order Claude Code 2.1.220 emits on the wire; a map would be re-sorted
	// alphabetically by encoding/json and change the serialized body bytes.
	reqBody := authorizationCodeExchangeRequest{
		GrantType:    "authorization_code",
		Code:         newCode,
		RedirectURI:  redirectURI,
		ClientID:     ClientID,
		CodeVerifier: pkceCodes.CodeVerifier,
		State:        state,
	}

	// A state fragment appended to the callback code takes precedence.
	if newState != "" {
		reqBody.State = newState
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	// log.Debugf("Token exchange request: %s", string(jsonBody))

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}
	applyClaudeOAuthAxiosHeaders(req)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close response body: %v", errClose)
		}
	}()

	body, err := readClaudeOAuthResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}
	// log.Debugf("Token response: %s", string(body))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}
	// log.Debugf("Token response: %s", string(body))

	var tokenResp tokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	deviceIDs, errDeviceIDs := GenerateDeviceIDPool()
	if errDeviceIDs != nil {
		return nil, errDeviceIDs
	}

	// Create token data.
	tokenData := ClaudeTokenData{
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		Email:            tokenResp.Account.EmailAddress,
		AccountUUID:      tokenResp.Account.UUID,
		OrganizationUUID: tokenResp.Organization.UUID,
		OrganizationName: tokenResp.Organization.Name,
		Expire:           time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	// Replay the native login companion lookups and let the profile response win
	// where it carries identity the token response omitted.
	if profile := o.inspectOAuthAccount(ctx, tokenResp.AccessToken); profile != nil {
		if value := strings.TrimSpace(profile.Account.UUID); value != "" {
			tokenData.AccountUUID = value
		}
		if value := strings.TrimSpace(profile.Account.Email); value != "" {
			tokenData.Email = value
		}
		if value := strings.TrimSpace(profile.Organization.UUID); value != "" {
			tokenData.OrganizationUUID = value
		}
		if value := strings.TrimSpace(profile.Organization.Name); value != "" {
			tokenData.OrganizationName = value
		}
	}

	// Create auth bundle.
	bundle := &ClaudeAuthBundle{
		TokenData:   tokenData,
		DeviceIDs:   deviceIDs,
		LastRefresh: time.Now().Format(time.RFC3339),
	}

	return bundle, nil
}

// RefreshTokens spends the refresh token at Claude's token endpoint, by the one refresh policy
// (internal/auth/tokenrefresh), and returns the new tokens. It does not look the account's profile up: the caller saves the rotated
// tokens first (FetchOAuthProfile after), so a lookup that fails or hangs never holds them off
// disk. account names the credential in logs.
func (o *ClaudeAuth) RefreshTokens(ctx context.Context, account, refreshToken string) (*ClaudeTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}
	if o == nil || o.httpClient == nil {
		return nil, fmt.Errorf("token refresh: HTTP client is nil")
	}
	answer, err := tokenrefresh.Refresh(ctx, tokenrefresh.Exchange{
		Provider:     "claude",
		Account:      account,
		RefreshToken: refreshToken,
		Send: func(ctx context.Context) (*http.Response, error) {
			body, errMarshal := json.Marshal(map[string]any{
				"client_id":     ClientID,
				"grant_type":    "refresh_token",
				"refresh_token": refreshToken,
				"scope":         ClaudeOAuthScope,
			})
			if errMarshal != nil {
				return nil, fmt.Errorf("failed to marshal request body: %w", errMarshal)
			}
			ctx = context.WithValue(ctx, claudeRefreshHandshakeTimeoutContextKey{}, claudeRefreshHandshakeTimeout)
			req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, RefreshTokenURL, strings.NewReader(string(body)))
			if errRequest != nil {
				return nil, fmt.Errorf("failed to create refresh request: %w", errRequest)
			}
			applyClaudeOAuthAxiosHeaders(req)
			return o.httpClient.Do(req)
		},
		Read: readClaudeOAuthResponseBody,
	})
	if err != nil {
		return nil, err
	}
	tokenData := &ClaudeTokenData{
		AccessToken:  answer.AccessToken,
		RefreshToken: answer.RefreshToken,
		Expire:       answer.ExpiresAt().Format(time.RFC3339),
	}
	return tokenData, nil
}

// CreateTokenStorage creates a new ClaudeTokenStorage from auth bundle and user info.
// This method converts the authentication bundle into a token storage structure
// suitable for persistence and later use.
//
// Parameters:
//   - bundle: The authentication bundle containing token data
//
// Returns:
//   - *ClaudeTokenStorage: A new token storage instance
func (o *ClaudeAuth) CreateTokenStorage(bundle *ClaudeAuthBundle) *ClaudeTokenStorage {
	storage := &ClaudeTokenStorage{
		AccessToken:      bundle.TokenData.AccessToken,
		RefreshToken:     bundle.TokenData.RefreshToken,
		LastRefresh:      bundle.LastRefresh,
		Email:            bundle.TokenData.Email,
		AccountUUID:      bundle.TokenData.AccountUUID,
		OrganizationUUID: bundle.TokenData.OrganizationUUID,
		OrganizationName: bundle.TokenData.OrganizationName,
		DeviceIDs:        append([]string(nil), bundle.DeviceIDs...),
		Expire:           bundle.TokenData.Expire,
	}

	return storage
}

// UpdateTokenStorage updates an existing token storage with new token data.
// This method refreshes the token storage with newly obtained access and refresh tokens,
// updating timestamps and expiration information.
//
// Parameters:
//   - storage: The existing token storage to update
//   - tokenData: The new token data to apply
func (o *ClaudeAuth) UpdateTokenStorage(storage *ClaudeTokenStorage, tokenData *ClaudeTokenData) {
	storage.AccessToken = tokenData.AccessToken
	storage.RefreshToken = tokenData.RefreshToken
	storage.LastRefresh = time.Now().Format(time.RFC3339)
	if tokenData.Email != "" {
		storage.Email = tokenData.Email
	}
	if tokenData.AccountUUID != "" {
		storage.AccountUUID = tokenData.AccountUUID
	}
	if tokenData.OrganizationUUID != "" {
		storage.OrganizationUUID = tokenData.OrganizationUUID
	}
	if tokenData.OrganizationName != "" {
		storage.OrganizationName = tokenData.OrganizationName
	}
	storage.Expire = tokenData.Expire
}
