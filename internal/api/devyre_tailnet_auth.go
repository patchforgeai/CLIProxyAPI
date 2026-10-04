package api

// devyre: tailnet passwordless access on the proxy API (management.tailnet-auth
// with proxy-api: true), the key-only /v1/ws variant, and anti-framing headers
// for the pages that load the keyless panel. The trust decision itself is
// internal/tailnetauth; management routes apply it in Handler.Middleware().
//
// Route coverage:
//   - /v1, /openai/v1, /backend-api/codex, /v1beta and the /v1/realtime fallback
//     authenticate through accessAuthMiddleware, which consults
//     devyreKeylessProxyAccess only after the API keys failed with a 401.
//   - /v1/ws (AttachWebsocketRoute, the AI Studio relay) uses
//     devyreKeyOnlyAuthMiddleware and is never keyless.
//   - RESP on the API port and /keep-alive use the management key or the local
//     password only; tailscale serve cannot carry RESP.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tailnetauth"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	log "github.com/sirupsen/logrus"
)

const (
	// devyreServerContextKey lets accessAuthMiddleware, which only receives the
	// access manager, read the live config of the server handling the request.
	devyreServerContextKey = "devyre.tailnet-auth.server"
	// devyreKeyOnlyContextKey marks a request whose route never accepts keyless access.
	devyreKeyOnlyContextKey = "devyre.tailnet-auth.key-only"
	// devyreKeylessAccessProvider is the accessProvider of a keyless request.
	devyreKeylessAccessProvider = "tailnet-auth"
)

// devyreTailnetContext stores the server on every request for devyreKeylessProxyAccess.
func (s *Server) devyreTailnetContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(devyreServerContextKey, s)
		c.Next()
	}
}

// devyreKeylessProxyAccess runs after the API keys rejected the request. It
// accepts the request without a key when management.tailnet-auth.proxy-api is
// on and the request is trusted, and attributes it to a stable per-device
// principal.
//
// A valid API key always wins: accessAuthMiddleware only calls this after
// manager.Authenticate failed with "no credentials" or "invalid credential", so
// configured clients keep their own principal, usage rows and isolation
// namespaces. userApiKey also seeds caller-scoped session isolation, the Claude
// MCP alias secret, the Codex prompt-cache key and the xAI reasoning-replay
// namespace, so the principal is never empty and is distinct per device. It need
// not be secret: the server derives it from verified identity, and only software
// inside the trust boundary can forge one.
func devyreKeylessProxyAccess(c *gin.Context, authErr *sdkaccess.AuthError) bool {
	if c == nil || c.Request == nil || authErr == nil || authErr.HTTPStatusCode() != http.StatusUnauthorized {
		return false
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials) &&
		!sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		return false
	}
	if c.GetBool(devyreKeyOnlyContextKey) {
		return false
	}
	value, ok := c.Get(devyreServerContextKey)
	if !ok {
		return false
	}
	server, ok := value.(*Server)
	if !ok || server == nil {
		return false
	}
	cfg := server.getConfig()
	if cfg == nil || !cfg.RemoteManagement.TailnetAuth.Enabled || !cfg.RemoteManagement.TailnetAuth.ProxyAPI {
		return false
	}
	path := c.Request.URL.Path
	decision := tailnetauth.Decide(tailnetauth.PolicyFromConfig(cfg), tailnetauth.FromHTTP(c.RemoteIP(), c.Request))
	if !decision.Trusted {
		log.WithFields(log.Fields{"reason": decision.Reason, "path": path}).Debug("tailnet-auth: proxy request needs an API key")
		return false
	}
	c.Set("userApiKey", decision.Principal())
	c.Set("accessProvider", devyreKeylessAccessProvider)
	c.Set("accessMetadata", map[string]string{"source": decision.Method})
	log.WithFields(log.Fields{
		"method": decision.Method,
		"login":  decision.Login,
		"device": decision.Device,
		"path":   path,
	}).Debug("tailnet-auth: proxy request trusted without an API key")
	return true
}

// devyreKeyOnlyAuthMiddleware is AuthMiddleware for routes that are never
// keyless, such as the /v1/ws provider relay.
func devyreKeyOnlyAuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	authenticate := AuthMiddleware(manager)
	return func(c *gin.Context) {
		c.Set(devyreKeyOnlyContextKey, true)
		authenticate(c)
	}
}

// devyreDenyFraming forbids framing the pages that load the panel, which acts
// with keyless admin rights on trusted devices.
func devyreDenyFraming(c *gin.Context) {
	c.Header("Content-Security-Policy", "frame-ancestors 'none'")
	c.Header("X-Frame-Options", "DENY")
}
