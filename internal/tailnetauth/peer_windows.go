//go:build windows

package tailnetauth

import (
	"errors"
	"net/netip"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIPHelper             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modIPHelper.NewProc("GetExtendedTcpTable")
)

// tcpTableOwnerPIDConnections is TCP_TABLE_OWNER_PID_CONNECTIONS: connected sockets and the pid
// that owns each, readable without special rights.
const tcpTableOwnerPIDConnections = 4

// mibTCPRowOwnerPID is MIB_TCPROW_OWNER_PID. Addresses are in network order; each port is in the
// low 16 bits, in network order too.
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  [4]byte
	LocalPort  uint32
	RemoteAddr [4]byte
	RemotePort uint32
	OwningPID  uint32
}

// mibTCP6RowOwnerPID is MIB_TCP6ROW_OWNER_PID.
type mibTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwningPID     uint32
}

func peerRunsAsThisUser(server, client netip.AddrPort) (bool, error) {
	pid, err := connectionOwner(server, client)
	if err != nil {
		return false, err
	}
	return processRunsAsThisUser(pid)
}

// connectionOwner is the pid that owns the client's socket: the row whose local end is client and
// whose remote end is server. A dual-stack listener lists IPv4 peers as mapped IPv6 rows, so both
// tables are searched.
func connectionOwner(server, client netip.AddrPort) (uint32, error) {
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		table, err := tcpTable(family)
		if err != nil {
			return 0, err
		}
		rowSize := unsafe.Sizeof(mibTCPRowOwnerPID{})
		if family == windows.AF_INET6 {
			rowSize = unsafe.Sizeof(mibTCP6RowOwnerPID{})
		}
		if len(table) < 4 {
			continue
		}
		count := int(*(*uint32)(unsafe.Pointer(&table[0])))
		if count == 0 {
			continue
		}
		if uintptr(len(table)-4) < uintptr(count)*rowSize {
			return 0, errors.New("the TCP table is shorter than its row count")
		}
		rows := unsafe.Pointer(&table[4])
		for i := 0; i < count; i++ {
			var local, remote netip.AddrPort
			var pid uint32
			if family == windows.AF_INET {
				row := (*mibTCPRowOwnerPID)(unsafe.Add(rows, uintptr(i)*rowSize))
				local = netip.AddrPortFrom(netip.AddrFrom4(row.LocalAddr), networkPort(row.LocalPort))
				remote = netip.AddrPortFrom(netip.AddrFrom4(row.RemoteAddr), networkPort(row.RemotePort))
				pid = row.OwningPID
			} else {
				row := (*mibTCP6RowOwnerPID)(unsafe.Add(rows, uintptr(i)*rowSize))
				local = netip.AddrPortFrom(netip.AddrFrom16(row.LocalAddr).Unmap(), networkPort(row.LocalPort))
				remote = netip.AddrPortFrom(netip.AddrFrom16(row.RemoteAddr).Unmap(), networkPort(row.RemotePort))
				pid = row.OwningPID
			}
			if local == client && remote == server {
				return pid, nil
			}
		}
	}
	return 0, errPeerNotFound
}

// tcpTable is GetExtendedTcpTable's MIB_TCP(6)TABLE_OWNER_PID for family, grown until it fits
// (connections come and go between the calls).
func tcpTable(family uint32) ([]byte, error) {
	size := uint32(64 * 1024)
	for attempt := 0; attempt < 8; attempt++ {
		table := make([]byte, size)
		result, _, _ := procGetExtendedTcpTable.Call(
			uintptr(unsafe.Pointer(&table[0])),
			uintptr(unsafe.Pointer(&size)),
			0,
			uintptr(family),
			tcpTableOwnerPIDConnections,
			0,
		)
		switch windows.Errno(result) {
		case windows.ERROR_SUCCESS:
			return table[:size], nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 16 * 1024 // and a little room for connections opened meanwhile
		default:
			return nil, windows.Errno(result)
		}
	}
	return nil, errors.New("the TCP table kept growing")
}

// networkPort reads a port stored in network order in the low 16 bits of a DWORD.
func networkPort(value uint32) uint16 {
	return uint16(value>>8&0xff | value&0xff<<8)
}

// processRunsAsThisUser compares the user of pid's token with this process's. A process that may
// not be opened (another user's, a protected one) is not this user's.
func processRunsAsThisUser(pid uint32) (bool, error) {
	if pid == uint32(os.Getpid()) {
		return true, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var token windows.Token
	if err = windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = token.Close() }()
	peer, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	self, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	return windows.EqualSid(peer.User.Sid, self.User.Sid), nil
}
