package codex

import "fmt"

// LoopbackRedirectURI is the redirect for a callback listener on port. auth.openai.com accepts
// any port only for the loopback IP literal (RFC 8252 §7.3): http://localhost with a port
// other than 1455 is refused with "invalid_authorize_request", and so is [::1].
func LoopbackRedirectURI(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", port)
}

// GenerateAuthURLWithRedirect is GenerateAuthURL with the redirect a caller's own callback
// listener answers on. The token exchange must use the same redirect
// (ExchangeCodeForTokensWithRedirect).
func (o *CodexAuth) GenerateAuthURLWithRedirect(state string, pkceCodes *PKCECodes, redirectURI string) (string, error) {
	if redirectURI == "" {
		return "", fmt.Errorf("redirect URI is required")
	}
	return o.generateAuthURL(state, pkceCodes, redirectURI)
}
