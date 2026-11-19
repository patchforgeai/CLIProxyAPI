package antigravity

import "fmt"

// LoopbackRedirectURI is the redirect for a callback listener on port, in the form of the fixed
// one (CallbackPort, /oauth-callback). Antigravity's client is an installed application's, whose
// loopback redirects Google accepts on any port (RFC 8252 §7.3), as upstream's own
// --oauth-callback-port relies on, so a caller with its own listener on an OS-assigned port never
// collides with another program holding 51121.
func LoopbackRedirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d/oauth-callback", port)
}
