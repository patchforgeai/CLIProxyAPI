package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
)

func TestDevyreLoopbackRedirectURI(t *testing.T) {
	if got := LoopbackRedirectURI(51234); got != "http://localhost:51234/callback" {
		t.Fatalf("LoopbackRedirectURI() = %q", got)
	}
}

func TestDevyreGenerateAuthURLWithRedirect(t *testing.T) {
	auth := &ClaudeAuth{}
	pkce := &PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}

	fixed, _, errFixed := auth.GenerateAuthURL("state-value", pkce)
	if errFixed != nil {
		t.Fatal(errFixed)
	}
	moved, state, errMoved := auth.GenerateAuthURLWithRedirect("state-value", pkce, LoopbackRedirectURI(51234))
	if errMoved != nil {
		t.Fatal(errMoved)
	}
	if state != "state-value" {
		t.Fatalf("state = %q", state)
	}
	fixedURL, _ := url.Parse(fixed)
	movedURL, _ := url.Parse(moved)
	if got := fixedURL.Query().Get("redirect_uri"); got != RedirectURI {
		t.Fatalf("default redirect_uri = %q, want upstream's %q", got, RedirectURI)
	}
	if got := movedURL.Query().Get("redirect_uri"); got != "http://localhost:51234/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	// Only the redirect differs.
	fixedQuery, movedQuery := fixedURL.Query(), movedURL.Query()
	fixedQuery.Del("redirect_uri")
	movedQuery.Del("redirect_uri")
	if fixedQuery.Encode() != movedQuery.Encode() || fixedURL.Host != movedURL.Host || fixedURL.Path != movedURL.Path {
		t.Fatalf("authorize URLs differ beyond the redirect:\n%s\n%s", fixed, moved)
	}

	if _, _, errEmpty := auth.GenerateAuthURLWithRedirect("state-value", pkce, ""); errEmpty == nil {
		t.Fatal("an empty redirect was accepted")
	}
}

func TestDevyreExchangeCodeForTokensWithRedirect(t *testing.T) {
	var exchanged map[string]string
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.String() {
				case TokenURL:
					body, errRead := io.ReadAll(req.Body)
					if errRead != nil {
						t.Fatal(errRead)
					}
					if errJSON := json.Unmarshal(body, &exchanged); errJSON != nil {
						t.Fatal(errJSON)
					}
					return jsonResponse(req, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`), nil
				case ProfileURL:
					return jsonResponse(req, `{"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email":"user@example.com"}}`), nil
				case RolesURL:
					return jsonResponse(req, `{"roles":[]}`), nil
				}
				t.Fatalf("unexpected request %s", req.URL)
				return nil, nil
			}),
		},
	}
	redirect := LoopbackRedirectURI(51234)
	bundle, errExchange := auth.ExchangeCodeForTokensWithRedirect(context.Background(), "auth-code", "state-value", &PKCECodes{CodeVerifier: "verifier"}, redirect)
	if errExchange != nil {
		t.Fatal(errExchange)
	}
	if exchanged["redirect_uri"] != redirect {
		t.Fatalf("exchange redirect_uri = %q, want %q", exchanged["redirect_uri"], redirect)
	}
	if exchanged["code"] != "auth-code" || exchanged["code_verifier"] != "verifier" || exchanged["client_id"] != ClientID {
		t.Fatalf("exchange body = %v", exchanged)
	}
	if bundle.TokenData.Email != "user@example.com" {
		t.Fatalf("email = %q", bundle.TokenData.Email)
	}

	if _, errEmpty := auth.ExchangeCodeForTokensWithRedirect(context.Background(), "auth-code", "state-value", &PKCECodes{CodeVerifier: "verifier"}, ""); errEmpty == nil {
		t.Fatal("an empty redirect was accepted")
	}
}
