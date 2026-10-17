package tailnetauth

// devyre: which OS user a direct loopback peer runs as, so management.tailnet-auth.allow-local
// trusts this server's own user rather than every program on the machine (decideLocal).

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// errPeerNotFound: the OS lists no such connection (it closed, or it is not local).
var errPeerNotFound = errors.New("the connection is not in the system's TCP table")

// PeerRunsAsThisUser reports whether the process at the client end of the TCP connection from
// client to server, both on this machine, runs as the same OS user as this server. The OS says
// so without special rights on Windows (the TCP table's owning process, then that process's
// token) and on Linux (the uid of the client's socket in /proc/net/tcp); elsewhere the answer is
// errors.ErrUnsupported. A process this server may not inspect is another user's.
func PeerRunsAsThisUser(server, client netip.AddrPort) (bool, error) {
	server = netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
	client = netip.AddrPortFrom(client.Addr().Unmap(), client.Port())
	return peerRunsAsThisUser(server, client)
}

// peerCheck returns the request's PeerIsThisUser: a lookup run at most once, and only when a
// decision depends on it. Nil when the request did not arrive over a TCP connection.
func peerCheck(r *http.Request) func() (bool, error) {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil {
		return nil
	}
	server, errServer := netip.ParseAddrPort(local.String())
	client, errClient := netip.ParseAddrPort(r.RemoteAddr)
	if errServer != nil || errClient != nil {
		return nil
	}
	var (
		done bool
		same bool
		err  error
	)
	return func() (bool, error) {
		if !done {
			done = true
			same, err = PeerRunsAsThisUser(server, client)
		}
		return same, err
	}
}

// procNetOwner finds, in the text of /proc/net/tcp or /proc/net/tcp6, the socket whose local
// address is from and whose remote address is to, and returns the uid that owns it. order is how
// the kernel printed each 32-bit word of an address: the machine's native order.
func procNetOwner(table []byte, from, to netip.AddrPort, order binary.ByteOrder) (uint32, bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(table))
	for first := true; scanner.Scan(); first = false {
		fields := strings.Fields(scanner.Text())
		if first || len(fields) < 8 {
			continue // the header line, or a line too short to hold a uid
		}
		local, errLocal := parseProcNetAddr(fields[1], order)
		remote, errRemote := parseProcNetAddr(fields[2], order)
		if errLocal != nil || errRemote != nil {
			return 0, false, fmt.Errorf("unreadable TCP table line %q", scanner.Text())
		}
		if local != from || remote != to {
			continue
		}
		uid, errUID := strconv.ParseUint(fields[7], 10, 32)
		if errUID != nil {
			return 0, false, fmt.Errorf("unreadable uid %q", fields[7])
		}
		return uint32(uid), true, nil
	}
	return 0, false, scanner.Err()
}

// parseProcNetAddr parses "0100007F:1F90" (IPv4) or 32 hex digits and a port (IPv6), the address
// unmapped so it compares with what the HTTP server reports.
func parseProcNetAddr(text string, order binary.ByteOrder) (netip.AddrPort, error) {
	hexAddr, hexPort, found := strings.Cut(text, ":")
	if !found {
		return netip.AddrPort{}, errors.New("no port")
	}
	port, errPort := strconv.ParseUint(hexPort, 16, 16)
	if errPort != nil {
		return netip.AddrPort{}, errPort
	}
	raw, errHex := hex.DecodeString(hexAddr)
	if errHex != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, errors.New("bad address")
	}
	// Each 32-bit word was printed as a number in the machine's order: undo that per word.
	ip := make([]byte, len(raw))
	for word := 0; word < len(raw); word += 4 {
		order.PutUint32(ip[word:], binary.BigEndian.Uint32(raw[word:]))
	}
	addr, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}
