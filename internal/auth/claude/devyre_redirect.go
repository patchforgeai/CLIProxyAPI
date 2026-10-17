package claude

import (
	"context"
	"fmt"
)

// LoopbackRedirectURI is the redirect for a callback listener on port: the form Claude Code
// itself sends, whose own listener takes an OS-assigned port for every sign-in. The
// authorization server therefore accepts any port here (RFC 8252 §7.3).
func LoopbackRedirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d/callback", port)
}

// GenerateAuthURLWithRedirect is GenerateAuthURL with the redirect a caller's own callback
// listener answers on. The token exchange must use the same redirect
// (ExchangeCodeForTokensWithRedirect).
func (o *ClaudeAuth) GenerateAuthURLWithRedirect(state string, pkceCodes *PKCECodes, redirectURI string) (string, string, error) {
	if redirectURI == "" {
		return "", "", fmt.Errorf("redirect URI is required")
	}
	return o.generateAuthURL(state, pkceCodes, redirectURI)
}

// ExchangeCodeForTokensWithRedirect is ExchangeCodeForTokens for a sign-in started with
// GenerateAuthURLWithRedirect: the provider binds the code to the redirect it was issued for.
func (o *ClaudeAuth) ExchangeCodeForTokensWithRedirect(ctx context.Context, code, state string, pkceCodes *PKCECodes, redirectURI string) (*ClaudeAuthBundle, error) {
	if redirectURI == "" {
		return nil, fmt.Errorf("redirect URI is required for token exchange")
	}
	return o.exchangeCodeForTokens(ctx, code, state, pkceCodes, redirectURI)
}
