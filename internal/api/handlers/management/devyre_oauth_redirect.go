package management

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// devyreRedirectPortQuery is the opt-in query parameter of the Claude and Codex OAuth starts
// (GET /v8/management/oauth/auth-url?provider=claude|codex&redirect_port=<port>, and the
// deprecated /v0 routes, which share the handlers). With it, the authorize URL and the token
// exchange use a loopback redirect on that port instead of the provider's fixed one (54545,
// 1455), so a caller that runs its own callback listener on an OS-assigned port never collides
// with another program holding the fixed port. Without it nothing changes.
const devyreRedirectPortQuery = "redirect_port"

// devyreOAuthRedirect reads redirect_port. It returns "" when the parameter is absent (the
// provider's fixed redirect), the redirect URI that loopbackURI builds for the port, or writes a
// 400 and returns ok=false when the value is not a port, or comes with is_webui (whose callback
// forwarder listens on the fixed port).
func devyreOAuthRedirect(c *gin.Context, loopbackURI func(port int) string) (redirectURI string, ok bool) {
	raw, present := c.GetQuery(devyreRedirectPortQuery)
	if !present {
		return "", true
	}
	port, errPort := devyreParsePort(raw)
	if errPort != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid redirect_port"})
		return "", false
	}
	if isWebUIRequest(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "redirect_port cannot be combined with is_webui"})
		return "", false
	}
	return loopbackURI(port), true
}

// devyreParsePort accepts plain decimal digits for a port from 1 to 65535.
func devyreParsePort(raw string) (int, error) {
	if raw == "" || len(raw) > 5 || strings.TrimLeft(raw, "0123456789") != "" {
		return 0, fmt.Errorf("not a port: %q", raw)
	}
	port, errAtoi := strconv.Atoi(raw)
	if errAtoi != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("not a port: %q", raw)
	}
	return port, nil
}

func devyreClaudeAuthURL(auth *claude.ClaudeAuth, state string, pkceCodes *claude.PKCECodes, redirectURI string) (string, string, error) {
	if redirectURI == "" {
		return auth.GenerateAuthURL(state, pkceCodes)
	}
	return auth.GenerateAuthURLWithRedirect(state, pkceCodes, redirectURI)
}

func devyreClaudeExchange(ctx context.Context, auth *claude.ClaudeAuth, code, state string, pkceCodes *claude.PKCECodes, redirectURI string) (*claude.ClaudeAuthBundle, error) {
	if redirectURI == "" {
		return auth.ExchangeCodeForTokens(ctx, code, state, pkceCodes)
	}
	return auth.ExchangeCodeForTokensWithRedirect(ctx, code, state, pkceCodes, redirectURI)
}

// devyreCodexRedirectService is what a Codex OAuth service needs for a moved redirect;
// codex.CodexAuth has both methods.
type devyreCodexRedirectService interface {
	GenerateAuthURLWithRedirect(state string, pkceCodes *codex.PKCECodes, redirectURI string) (string, error)
	ExchangeCodeForTokensWithRedirect(ctx context.Context, code, redirectURI string, pkceCodes *codex.PKCECodes) (*codex.CodexAuthBundle, error)
}

func devyreCodexAuthURL(service codexOAuthService, state string, pkceCodes *codex.PKCECodes, redirectURI string) (string, error) {
	if redirectURI == "" {
		return service.GenerateAuthURL(state, pkceCodes)
	}
	moved, ok := service.(devyreCodexRedirectService)
	if !ok {
		return "", fmt.Errorf("the Codex OAuth service cannot move the redirect")
	}
	return moved.GenerateAuthURLWithRedirect(state, pkceCodes, redirectURI)
}

func devyreCodexExchange(ctx context.Context, service codexOAuthService, code string, pkceCodes *codex.PKCECodes, redirectURI string) (*codex.CodexAuthBundle, error) {
	if redirectURI == "" {
		return service.ExchangeCodeForTokens(ctx, code, pkceCodes)
	}
	moved, ok := service.(devyreCodexRedirectService)
	if !ok {
		return nil, fmt.Errorf("the Codex OAuth service cannot move the redirect")
	}
	return moved.ExchangeCodeForTokensWithRedirect(ctx, code, redirectURI, pkceCodes)
}
