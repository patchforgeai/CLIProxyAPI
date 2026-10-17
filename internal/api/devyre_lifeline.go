package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/netip"
	"regexp"

	"github.com/gin-gonic/gin"
)

// LifelineChallengeHeader carries a supervisor's challenge on GET /keep-alive. The answer proves
// that the process on the port holds the local password, without the password being sent: a
// supervisor that has not yet verified who answers never hands its secret to another program.
const LifelineChallengeHeader = "X-CPA-Lifeline-Challenge"

// lifelineProofContext separates the proof from any other use of the local password.
const lifelineProofContext = "cpa-lifeline-proof/v1"

// lifelineChallengePattern accepts base64url text of at least 128 bits.
var lifelineChallengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22,128}$`)

// devyreServerState holds the fork's own server settings, so server.go needs one field for all of
// them.
type devyreServerState struct {
	// listenReport learns the outcome of Start's listen (WithDevyreListenReport).
	listenReport func(address string, err error)
}

// LifelineProof is the answer to a lifeline challenge: base64url (unpadded) HMAC-SHA256, keyed by
// the local password, of the proof context, the server address the request reached
// ("127.0.0.1:8317") and the challenge, one per line. Binding the address keeps a proof from being
// relayed from a server on another port.
func LifelineProof(password, address, challenge string) string {
	mac := hmac.New(sha256.New, []byte(password))
	_, _ = mac.Write([]byte(lifelineProofContext + "\n" + address + "\n" + challenge))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// WithDevyreKeepAliveProbe registers GET /keep-alive without the idle shutdown that
// WithKeepAliveEndpoint adds. The endpoint still answers only the local password, so a
// supervisor can prove that the process on the port is the one it started, while the stdin
// lifeline (internal/cmd/devyre_lifeline.go) bounds the process's life instead of the pings. It
// also answers lifeline challenges (LifelineChallengeHeader) with LifelineProof.
func WithDevyreKeepAliveProbe() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.devyreKeepAliveProbe = true
	}
}

// WithDevyreListenReport has Start call report once: with the bound address right before the
// server accepts its first connection, or with the configured address and the error when the
// listener cannot be created. The stdin lifeline prints it for the supervisor.
func WithDevyreListenReport(report func(address string, err error)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.devyreListenReport = report
	}
}

func (s *Server) devyreEnableKeepAliveProbe() {
	if s.keepAliveEnabled {
		// WithKeepAliveEndpoint already serves the route, with its idle limit.
		return
	}
	s.engine.GET("/keep-alive", s.devyreLifelineKeepAlive)
}

// devyreReportListen passes Start's listen outcome to WithDevyreListenReport's function.
func (s *Server) devyreReportListen(address string, err error) {
	if report := s.devyre.listenReport; report != nil {
		report(address, err)
	}
}

// devyreLifelineKeepAlive answers a lifeline challenge with the proof, and anything else as
// upstream's /keep-alive does (200 to the local password, 401 otherwise).
func (s *Server) devyreLifelineKeepAlive(c *gin.Context) {
	challenges := c.Request.Header.Values(LifelineChallengeHeader)
	if len(challenges) == 0 {
		s.handleKeepAlive(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	if len(challenges) != 1 || !lifelineChallengePattern.MatchString(challenges[0]) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid challenge"})
		return
	}
	// The proof is for the supervisor on this machine: never for a browser page, never remote.
	if c.GetHeader("Origin") != "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "challenges are not answered for browsers"})
		return
	}
	peer, errPeer := netip.ParseAddr(c.RemoteIP())
	if errPeer != nil || !peer.Unmap().IsLoopback() {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "challenges are answered on loopback only"})
		return
	}
	local, ok := c.Request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil || s.localPassword == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "the server address is unknown"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "proof": LifelineProof(s.localPassword, local.String(), challenges[0])})
}
