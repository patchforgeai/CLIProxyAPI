package management

// devyre: passwordless management access for allowed tailnet devices and this PC
// (management.tailnet-auth, decided by internal/tailnetauth). Middleware() asks
// devyreTailnetAuthorize first, so every route behind it, on /v0 and /v8 and the
// plugin routes served through pluginManagementNoRoute, is either trusted or
// still needs the key. OAuth callbacks and /v0/resource/plugins/* never pass
// through Middleware() and stay exactly as they were.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tailnetauth"
	log "github.com/sirupsen/logrus"
)

// devyreAuthSessionContextKey holds the tailnetauth.Decision of a trusted request.
const devyreAuthSessionContextKey = "devyre.management.auth-session"

// devyreTailnetAuthorize reports whether the request is trusted and may skip the
// management key. A trusted request never touches the failure counter or the
// ban list, and a key it carries is ignored. Trust never widens what the key
// check allows: it needs a configured management key (without one the API is
// disabled), and with management.allow-remote off only a loopback client can be
// trusted. Trusted responses drop the CORS headers corsMiddleware set and are
// never cached, so another origin can never read a keyless response.
func (h *Handler) devyreTailnetAuthorize(c *gin.Context) bool {
	if h == nil || c == nil || c.Request == nil {
		return false
	}
	h.mu.Lock()
	policy := tailnetauth.PolicyFromConfig(h.cfg)
	allowRemote := h.allowRemoteOverride || (h.cfg != nil && h.cfg.RemoteManagement.AllowRemote)
	keyConfigured := h.envSecret != "" || (h.cfg != nil && h.cfg.RemoteManagement.SecretKey != "")
	h.mu.Unlock()
	if !policy.Enabled || !keyConfigured {
		return false
	}
	path := c.Request.URL.Path
	decision := tailnetauth.Decide(policy, tailnetauth.FromHTTP(c.RemoteIP(), c.Request))
	if !decision.Trusted {
		log.WithFields(log.Fields{"reason": decision.Reason, "path": path}).Debug("tailnet-auth: management request needs the key")
		return false
	}
	if clientIP := c.ClientIP(); !allowRemote && clientIP != "127.0.0.1" && clientIP != "::1" {
		log.WithField("path", path).Debug("tailnet-auth: management request needs the key: management.allow-remote is off")
		return false
	}
	c.Set(devyreAuthSessionContextKey, decision)
	header := c.Writer.Header()
	for name := range header {
		if strings.HasPrefix(name, "Access-Control-") {
			header.Del(name)
		}
	}
	header.Set("Cache-Control", "no-store")
	log.WithFields(log.Fields{
		"method": decision.Method,
		"login":  decision.Login,
		"device": decision.Device,
		"path":   path,
	}).Debug("tailnet-auth: management request trusted without a key")
	return true
}

// authSessionResponse is the body of GET /v8/management/auth/session. Login and
// Device are empty unless Method is "tailnet".
type authSessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	Method        string `json:"method"`
	Login         string `json:"login"`
	Device        string `json:"device"`
}

// GetAuthSession handles GET /v8/management/auth/session. It runs behind
// Middleware(), so reaching it means the request is authenticated: by tailnet
// trust, by local trust, or by a management key. The panel probes it without an
// Authorization header to decide whether to skip the login form; an untrusted
// probe gets Middleware's 401, which never counts toward the ban.
func (h *Handler) GetAuthSession(c *gin.Context) {
	response := authSessionResponse{Authenticated: true, Method: tailnetauth.MethodKey}
	if value, ok := c.Get(devyreAuthSessionContextKey); ok {
		if decision, okDecision := value.(tailnetauth.Decision); okDecision && decision.Trusted {
			response.Method = decision.Method
			if decision.Method == tailnetauth.MethodTailnet {
				response.Login, response.Device = decision.Login, decision.Device
			}
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, response)
}
