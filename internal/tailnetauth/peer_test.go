package tailnetauth

import (
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"testing"
)

// Lines as Linux prints them on a little-endian machine: 127.0.0.1:43210 -> 127.0.0.1:8317 owned
// by uid 1000, the server's own side owned by uid 0, an IPv6 pair and an IPv4-mapped pair.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:207D 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:A8CA 0100007F:207D 01 00000000:00000000 00:00000000 00000000  1000        0 22222 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:207D 0100007F:A8CA 01 00000000:00000000 00:00000000 00000000     0        0 33333 1 0000000000000000 20 4 30 10 -1
`

const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:A8CB 00000000000000000000000001000000:207D 01 00000000:00000000 00:00000000 00000000  1001        0 44444 1
   1: 0000000000000000FFFF00000100007F:A8CC 0000000000000000FFFF00000100007F:207D 01 00000000:00000000 00:00000000 00000000  1002        0 55555 1
`

func TestProcNetOwner(t *testing.T) {
	server := netip.MustParseAddrPort("127.0.0.1:8317")
	cases := []struct {
		name   string
		table  string
		client string
		uid    uint32
		found  bool
	}{
		{name: "IPv4", table: procNetTCP, client: "127.0.0.1:43210", uid: 1000, found: true},
		{name: "not listed", table: procNetTCP, client: "127.0.0.1:43299"},
		{name: "IPv6", table: procNetTCP6, client: "[::1]:43211", uid: 1001, found: true},
		{name: "IPv4-mapped in tcp6", table: procNetTCP6, client: "127.0.0.1:43212", uid: 1002, found: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client := netip.MustParseAddrPort(tt.client)
			to := server
			if client.Addr().Is6() {
				to = netip.MustParseAddrPort("[::1]:8317")
			}
			uid, found, err := procNetOwner([]byte(tt.table), client, to, binary.LittleEndian)
			if err != nil || found != tt.found || uid != tt.uid {
				t.Fatalf("procNetOwner() = %d, %v, %v; want %d, %v", uid, found, err, tt.uid, tt.found)
			}
		})
	}
	// The server's own socket for the same connection belongs to its user, not the client's.
	if uid, found, _ := procNetOwner([]byte(procNetTCP), server, netip.MustParseAddrPort("127.0.0.1:43210"), binary.LittleEndian); !found || uid != 0 {
		t.Fatalf("the server side: uid %d found %v", uid, found)
	}
	// A big-endian machine prints each word in network order.
	bigEndian := strings.NewReplacer("0100007F", "7F000001", ":A8CA", ":A8CA").Replace(procNetTCP)
	if uid, found, err := procNetOwner([]byte(bigEndian), netip.MustParseAddrPort("127.0.0.1:43210"), server, binary.BigEndian); err != nil || !found || uid != 1000 {
		t.Fatalf("big-endian table: %d, %v, %v", uid, found, err)
	}
	if _, _, err := procNetOwner([]byte("header\n   0: zz:207D 0100007F:207D 01 0 0 0 1000\n"), server, server, binary.LittleEndian); err == nil {
		t.Fatal("a garbled address was accepted")
	}
}

// loopbackPair connects to a listener on addr from this process and returns both ends' addresses
// as the server sees them.
func loopbackPair(t *testing.T, addr string) (server, client netip.AddrPort) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("no %s listener here: %v", addr, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept == nil {
			accepted <- conn
		}
		close(accepted)
	}()
	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close() })
	conn, ok := <-accepted
	if !ok {
		t.Fatal("no connection accepted")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return netip.MustParseAddrPort(conn.LocalAddr().String()), netip.MustParseAddrPort(conn.RemoteAddr().String())
}

func peerLookupSupported() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "linux"
}

// A real loopback connection from this process: the OS says it is this user's (where it says at
// all), and a connection it does not list is no one's.
func TestPeerRunsAsThisUser(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			server, client := loopbackPair(t, addr)
			same, err := PeerRunsAsThisUser(server, client)
			if !peerLookupSupported() {
				if !errors.Is(err, errors.ErrUnsupported) || same {
					t.Fatalf("PeerRunsAsThisUser() = %v, %v; want ErrUnsupported", same, err)
				}
				return
			}
			if err != nil || !same {
				t.Fatalf("PeerRunsAsThisUser() = %v, %v; want this user", same, err)
			}
			// The same ports the other way round name the server's socket, not the client's.
			stray := netip.AddrPortFrom(client.Addr(), client.Port()+1)
			if same, err = PeerRunsAsThisUser(server, stray); same || !errors.Is(err, errPeerNotFound) {
				t.Fatalf("a connection that does not exist: %v, %v", same, err)
			}
		})
	}
}

// FromHTTP gives a request that came over a real connection the lookup; one built in memory has
// no peer to look up.
func TestFromHTTPLooksUpTheConnectionsPeer(t *testing.T) {
	if got := FromHTTP("127.0.0.1", httptest.NewRequest(http.MethodGet, "/", nil)); got.PeerIsThisUser != nil {
		t.Fatal("an in-memory request got a peer lookup")
	}
	results := make(chan [2]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check := FromHTTP("127.0.0.1", r).PeerIsThisUser
		if check == nil {
			results <- [2]any{false, errors.New("no lookup")}
			return
		}
		same, err := check()
		results <- [2]any{same, err}
	}))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	got := <-results
	same, _ := got[0].(bool)
	lookupErr, _ := got[1].(error)
	if peerLookupSupported() && (!same || lookupErr != nil) {
		t.Fatalf("lookup = %v, %v; want this user", same, lookupErr)
	}
	if !peerLookupSupported() && !errors.Is(lookupErr, errors.ErrUnsupported) {
		t.Fatalf("lookup error = %v, want ErrUnsupported", lookupErr)
	}
}
