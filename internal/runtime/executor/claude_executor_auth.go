package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	claudeAccountProfileCheckedAtKey = "claude_account_profile_checked_at"
	claudeAccountProfileTimeout      = 10 * time.Second
)

type claudeOAuthProfileFetcher func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error)

func (e *ClaudeExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	apiKey, _ := claudeCreds(auth)
	if !isClaudeOAuthToken(apiKey) || auth == nil {
		return false
	}
	if !claudeauth.HasCanonicalDeviceIDPool(claudeauth.ReadDeviceIDPool(&auth.Metadata)) {
		return true
	}
	return helps.ClaudeCredentialAccountUUID(auth) == ""
}

func isClaudeSetupToken(auth *cliproxyauth.Auth, apiKey string) bool {
	if !isClaudeOAuthToken(apiKey) || auth == nil {
		return false
	}
	if skip, _ := auth.Metadata["skip_account_profile"].(bool); skip {
		return true
	}
	if isSetup, _ := auth.Metadata["is_setup_token"].(bool); isSetup {
		return true
	}
	if isSetup, _ := auth.Metadata["setup_token"].(bool); isSetup {
		return true
	}
	if kind := strings.ToLower(auth.Attributes["auth_kind"]); kind == "setup_token" || kind == "setup-token" {
		return true
	}
	scopes := strings.ToLower(claudeauth.ReadMetadataString(&auth.Metadata, "scopes"))
	if scopes == "" {
		scopes = strings.ToLower(claudeauth.ReadMetadataString(&auth.Metadata, "scope"))
	}
	if scopes != "" && !strings.Contains(scopes, "user:profile") && !strings.Contains(scopes, "user:office") {
		return true
	}
	return false
}

func isClaudeOAuthScope403(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "403 forbidden") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "permission_error") ||
		strings.Contains(msg, "scope requirement") ||
		strings.Contains(msg, "insufficient_scope") ||
		strings.Contains(msg, "user:profile") ||
		strings.Contains(msg, "user:office")
}

func (e *ClaudeExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	apiKey, _ := claudeCreds(auth)
	claudeauth.EnsureMetadataMap(&auth.Metadata)
	if _, errDeviceIDs := helps.EnsureClaudeCredentialDevicePoolRequired(ctx, auth); errDeviceIDs != nil {
		return nil, errDeviceIDs
	}
	if helps.ClaudeCredentialAccountUUID(auth) != "" {
		return auth, nil
	}

	if isClaudeSetupToken(auth, apiKey) {
		seed := helps.ClaudeCLIAuthIdentitySeed(auth)
		if seed == "" {
			seed = "claude-setup-token|" + apiKey
		}
		claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", helps.StableClaudeCLIAccountUUID(seed))
		claudeauth.StoreMetadataString(&auth.Metadata, claudeAccountProfileCheckedAtKey, time.Now().UTC().Format(time.RFC3339))
		return auth, nil
	}

	profile, errProfile := e.fetchClaudeOAuthProfile(ctx, auth, apiKey)
	if errProfile != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		if isClaudeOAuthScope403(errProfile) {
			log.Debugf("Claude OAuth account profile lookup returned 403 for auth %s: %v (falling back to stable credential identity)", auth.ID, errProfile)
			seed := helps.ClaudeCLIAuthIdentitySeed(auth)
			if seed == "" {
				seed = "claude-oauth-fallback|" + apiKey
			}
			claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", helps.StableClaudeCLIAccountUUID(seed))
			claudeauth.StoreMetadataString(&auth.Metadata, claudeAccountProfileCheckedAtKey, time.Now().UTC().Format(time.RFC3339))
			return auth, nil
		}
		return nil, fmt.Errorf("populate Claude OAuth account profile: %w", errProfile)
	}
	if profile == nil || strings.TrimSpace(profile.Account.UUID) == "" {
		log.Debugf("Claude OAuth account profile lookup returned empty account UUID for auth %s (falling back to stable credential identity)", auth.ID)
		seed := helps.ClaudeCLIAuthIdentitySeed(auth)
		if seed == "" {
			seed = "claude-oauth-fallback|" + apiKey
		}
		claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", helps.StableClaudeCLIAccountUUID(seed))
		claudeauth.StoreMetadataString(&auth.Metadata, claudeAccountProfileCheckedAtKey, time.Now().UTC().Format(time.RFC3339))
		return auth, nil
	}
	claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", profile.Account.UUID)
	claudeauth.StoreMetadataString(&auth.Metadata, "email", profile.Account.Email)
	claudeauth.StoreMetadataString(&auth.Metadata, "organization_uuid", profile.Organization.UUID)
	claudeauth.StoreMetadataString(&auth.Metadata, "organization_name", profile.Organization.Name)
	claudeauth.StoreMetadataString(&auth.Metadata, claudeAccountProfileCheckedAtKey, time.Now().UTC().Format(time.RFC3339))
	return auth, nil
}

func (e *ClaudeExecutor) fetchClaudeOAuthProfile(ctx context.Context, auth *cliproxyauth.Auth, apiKey string) (*claudeauth.OAuthProfile, error) {
	if e == nil {
		return nil, fmt.Errorf("fetch Claude OAuth profile: executor is nil")
	}
	if e.oauthProfileFetcher != nil {
		return e.oauthProfileFetcher(ctx, auth, apiKey)
	}
	if auth == nil {
		return nil, fmt.Errorf("fetch Claude OAuth profile: auth is nil")
	}
	profileCtx, cancelProfile := context.WithTimeout(ctx, claudeAccountProfileTimeout)
	defer cancelProfile()
	service := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	return service.FetchOAuthProfile(profileCtx, apiKey)
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	refreshToken := claudeauth.ReadMetadataString(&auth.Metadata, "refresh_token")
	if refreshToken == "" {
		refreshToken = claudeauth.ReadMetadataString(&auth.Metadata, "refreshToken")
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokens(ctx, auth.ID, refreshToken)
	if err != nil {
		return nil, err
	}
	claudeauth.EnsureMetadataMap(&auth.Metadata)
	claudeauth.StoreMetadataValue(&auth.Metadata, "access_token", td.AccessToken)
	claudeauth.StoreMetadataString(&auth.Metadata, "refresh_token", td.RefreshToken)
	claudeauth.StoreMetadataValue(&auth.Metadata, "expired", td.Expire)
	cliproxyauth.NoteSignInEnd(auth, td.RefreshTokenExpiresAt)
	claudeauth.StoreMetadataValue(&auth.Metadata, "type", "claude")
	claudeauth.StoreMetadataValue(&auth.Metadata, "last_refresh", time.Now().Format(time.RFC3339))
	return auth, nil
}

// AfterRefresh looks the account's profile up once the rotated tokens are saved, as Claude Code
// does after a refresh (cliproxyauth.RefreshFollower), and returns the credential with what
// changed of its identity; nil when nothing did. devyre: it ran inside the refresh before, so a
// slow or failed lookup held the new, single-use tokens off disk for another round trip. A
// failed lookup never erases the identity the credential has.
func (e *ClaudeExecutor) AfterRefresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || (e.cfg != nil && e.cfg.Home.Enabled) {
		return nil, nil
	}
	accessToken := claudeauth.ReadMetadataString(&auth.Metadata, "access_token")
	if accessToken == "" || isClaudeSetupToken(auth, accessToken) {
		return nil, nil
	}
	profile, errProfile := e.fetchClaudeOAuthProfile(ctx, auth, accessToken)
	if errProfile != nil {
		return nil, fmt.Errorf("fetch Claude OAuth profile after refreshing %s: %w", auth.ID, errProfile)
	}
	changed := false
	for key, value := range map[string]string{
		"email":             profile.Account.Email,
		"account_uuid":      profile.Account.UUID,
		"organization_uuid": profile.Organization.UUID,
		"organization_name": profile.Organization.Name,
	} {
		if value = strings.TrimSpace(value); value != "" && claudeauth.ReadMetadataString(&auth.Metadata, key) != value {
			claudeauth.StoreMetadataString(&auth.Metadata, key, value)
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return auth, nil
}
