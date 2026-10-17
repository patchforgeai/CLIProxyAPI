//go:build !windows && !linux

package tailnetauth

import (
	"errors"
	"net/netip"
)

// Elsewhere (macOS, the BSDs) no unprivileged, cgo-free call says which user owns a connection:
// local trust is never granted there.
func peerRunsAsThisUser(netip.AddrPort, netip.AddrPort) (bool, error) {
	return false, errors.ErrUnsupported
}
