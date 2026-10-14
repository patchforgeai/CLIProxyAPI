package codex

import (
	"net/url"
	"testing"
)

func TestDevyreLoopbackRedirectURI(t *testing.T) {
	if got := LoopbackRedirectURI(51234); got != "http://127.0.0.1:51234/auth/callback" {
		t.Fatalf("LoopbackRedirectURI() = %q", got)
	}
}

func TestDevyreGenerateAuthURLWithRedirect(t *testing.T) {
	auth := NewCodexAuth(nil)
	pkce := &PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}

	fixed, errFixed := auth.GenerateAuthURL("state-value", pkce)
	if errFixed != nil {
		t.Fatal(errFixed)
	}
	moved, errMoved := auth.GenerateAuthURLWithRedirect("state-value", pkce, LoopbackRedirectURI(51234))
	if errMoved != nil {
		t.Fatal(errMoved)
	}
	fixedURL, _ := url.Parse(fixed)
	movedURL, _ := url.Parse(moved)
	if got := fixedURL.Query().Get("redirect_uri"); got != RedirectURI {
		t.Fatalf("default redirect_uri = %q, want upstream's %q", got, RedirectURI)
	}
	if got := movedURL.Query().Get("redirect_uri"); got != "http://127.0.0.1:51234/auth/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	fixedQuery, movedQuery := fixedURL.Query(), movedURL.Query()
	fixedQuery.Del("redirect_uri")
	movedQuery.Del("redirect_uri")
	if fixedQuery.Encode() != movedQuery.Encode() || fixedURL.Host != movedURL.Host || fixedURL.Path != movedURL.Path {
		t.Fatalf("authorize URLs differ beyond the redirect:\n%s\n%s", fixed, moved)
	}

	if _, errEmpty := auth.GenerateAuthURLWithRedirect("state-value", pkce, ""); errEmpty == nil {
		t.Fatal("an empty redirect was accepted")
	}
}
