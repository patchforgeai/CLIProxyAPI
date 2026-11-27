package executor

import (
	"context"
	"errors"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyre: Claude's profile is looked up after a refresh's tokens are saved (RefreshFollower),
// with the new access token, and only a change of identity comes back.
func TestDevyreClaudeLooksTheProfileUpAfterTheRefresh(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	var _ cliproxyauth.RefreshFollower = executor
	profile := &claudeauth.OAuthProfile{}
	profile.Account.UUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	profile.Account.Email = "user@example.com"
	profile.Organization.UUID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	profile.Organization.Name = "Renamed Org"
	var askedWith string
	executor.oauthProfileFetcher = func(_ context.Context, _ *cliproxyauth.Auth, accessToken string) (*claudeauth.OAuthProfile, error) {
		askedWith = accessToken
		return profile, nil
	}
	auth := &cliproxyauth.Auth{ID: "claude-user@example.com.json", Provider: "claude", Metadata: map[string]any{
		"type": "claude", "access_token": "sk-ant-oat01-new", "email": "user@example.com",
		"account_uuid": profile.Account.UUID, "organization_uuid": profile.Organization.UUID, "organization_name": "Example Org",
	}}
	followed, err := executor.AfterRefresh(context.Background(), auth)
	if err != nil || followed == nil || followed.Metadata["organization_name"] != "Renamed Org" || askedWith != "sk-ant-oat01-new" {
		t.Fatalf("AfterRefresh() = %v, %v (asked with %q), want the renamed organization", followed, err, askedWith)
	}
	if followed, err = executor.AfterRefresh(context.Background(), followed); followed != nil || err != nil {
		t.Fatalf("an unchanged profile came back as a change: %v, %v", followed, err)
	}

	executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		return nil, errors.New("profile lookup failed with status 503")
	}
	if followed, err = executor.AfterRefresh(context.Background(), auth); followed != nil || err == nil {
		t.Fatalf("a failed lookup: %v, %v, want an error and nothing changed", followed, err)
	}
	if auth.Metadata["email"] != "user@example.com" {
		t.Fatal("a failed lookup erased the identity")
	}
}
