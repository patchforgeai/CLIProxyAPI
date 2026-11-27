package claude

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
)

// devyre: Claude's token answer says how long the sign-in lasts (refresh_token_expires_in): a
// sign-in keeps it as its end, a refresh keeps the earlier of the kept end and its own, and the
// token file holds it (refresh_token_expires_at).
func TestDevyreTheSignInKeepsTheEndItsAnswerGives(t *testing.T) {
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case TokenURL:
			return jsonResponse(req, `{"access_token":"access","refresh_token":"refresh","expires_in":28800,"refresh_token_expires_in":2592000,
				"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email_address":"user@example.com"}}`), nil
		case ProfileURL:
			return jsonResponse(req, `{"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email":"user@example.com"}}`), nil
		default:
			return jsonResponse(req, `{}`), nil
		}
	})}}
	before := time.Now()
	bundle, err := auth.ExchangeCodeForTokens(context.Background(), "code", "state", &PKCECodes{CodeVerifier: "verifier"})
	if err != nil {
		t.Fatal(err)
	}
	ends, ok := tokenrefresh.ParseEnd(bundle.TokenData.RefreshTokenExpiresAt)
	if !ok || ends.Before(before.Add(30*24*time.Hour-time.Second)) || ends.After(time.Now().Add(30*24*time.Hour)) {
		t.Fatalf("sign-in end = %q, want 30 days from now", bundle.TokenData.RefreshTokenExpiresAt)
	}
	storage := auth.CreateTokenStorage(bundle)
	if storage.RefreshTokenExpiresAt != bundle.TokenData.RefreshTokenExpiresAt {
		t.Fatalf("storage end = %q, want %q", storage.RefreshTokenExpiresAt, bundle.TokenData.RefreshTokenExpiresAt)
	}

	// A later end at a refresh never moves it; an earlier one does.
	later := tokenrefresh.FormatEnd(ends.Add(48 * time.Hour))
	auth.UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "a2", RefreshToken: "r2", RefreshTokenExpiresAt: later})
	if storage.RefreshTokenExpiresAt != bundle.TokenData.RefreshTokenExpiresAt {
		t.Fatalf("a refresh moved the end later: %q", storage.RefreshTokenExpiresAt)
	}
	earlier := tokenrefresh.FormatEnd(ends.Add(-48 * time.Hour))
	auth.UpdateTokenStorage(storage, &ClaudeTokenData{AccessToken: "a3", RefreshToken: "r3", RefreshTokenExpiresAt: earlier})
	if storage.RefreshTokenExpiresAt != earlier {
		t.Fatalf("end = %q, want the earlier %q", storage.RefreshTokenExpiresAt, earlier)
	}
}

func TestDevyreARefreshSaysWhenTheSignInEnds(t *testing.T) {
	tokenrefresh.Reset()
	t.Cleanup(tokenrefresh.Reset)
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, `{"access_token":"new","refresh_token":"next","expires_in":28800,"refresh_token_expires_in":86400}`), nil
	})}}
	tokenData, err := auth.RefreshTokens(context.Background(), "claude-user@example.com.json", "old")
	if err != nil {
		t.Fatal(err)
	}
	ends, ok := tokenrefresh.ParseEnd(tokenData.RefreshTokenExpiresAt)
	if !ok || ends.Before(time.Now().Add(24*time.Hour-2*time.Second)) || ends.After(time.Now().Add(24*time.Hour)) {
		t.Fatalf("end = %q, want a day from now", tokenData.RefreshTokenExpiresAt)
	}

	tokenrefresh.Reset()
	auth = &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, `{"access_token":"new","expires_in":28800}`), nil
	})}}
	tokenData, err = auth.RefreshTokens(context.Background(), "claude-user@example.com.json", "old")
	if err != nil || tokenData.RefreshTokenExpiresAt != "" {
		t.Fatalf("an answer without an end: %q, %v, want none", tokenData.RefreshTokenExpiresAt, err)
	}
}
