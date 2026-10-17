//go:build linux

package tailnetauth

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"net/netip"
	"os"
)

func peerRunsAsThisUser(server, client netip.AddrPort) (bool, error) {
	// A dual-stack listener lists IPv4 peers in tcp6 as mapped addresses, so both are searched.
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if errors.Is(err, fs.ErrNotExist) {
			continue // no IPv6
		}
		if err != nil {
			return false, err
		}
		uid, found, err := procNetOwner(data, client, server, binary.NativeEndian)
		if err != nil {
			return false, err
		}
		if found {
			return uid == uint32(os.Geteuid()), nil
		}
	}
	return false, errPeerNotFound
}
