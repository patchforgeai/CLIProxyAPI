package api

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
)

// devyre: only a wrong non-empty key counts toward the management ban (handler.go,
// AuthenticateManagementKey). RESP asks with an empty key before AUTH only to learn whether the IP
// is banned, so unauthenticated commands never lock a client out; five wrong passwords still do.
func TestRedisProtocol_OnlyWrongPasswordsCountTowardTheBan(t *testing.T) {
	const managementPassword = "test-management-password"

	t.Setenv("MANAGEMENT_PASSWORD", managementPassword)
	redisqueue.SetEnabled(false)
	t.Cleanup(func() { redisqueue.SetEnabled(false) })

	server := newTestServer(t)
	addr, stop := startRedisMuxListener(t, server)
	t.Cleanup(stop)

	dial := func() (net.Conn, *bufio.Reader) {
		t.Helper()
		conn, errDial := net.DialTimeout("tcp", addr, time.Second)
		if errDial != nil {
			t.Fatalf("failed to dial redis listener: %v", errDial)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		return conn, bufio.NewReader(conn)
	}
	expectError := func(conn net.Conn, reader *bufio.Reader, want string, args ...string) {
		t.Helper()
		if errWrite := writeTestRESPCommand(conn, args...); errWrite != nil {
			t.Fatalf("write %v: %v", args[0], errWrite)
		}
		msg, errRead := readTestRESPError(reader)
		if errRead != nil {
			t.Fatalf("read %v response: %v", args[0], errRead)
		}
		if !strings.HasPrefix(msg, want) {
			t.Fatalf("%v: got %q, want a reply starting with %q", args[0], msg, want)
		}
	}

	conn, reader := dial()
	for i := 0; i < 10; i++ {
		expectError(conn, reader, "NOAUTH", "RPOP", "usage")
	}
	expectError(conn, reader, "ERR wrong number of arguments", "AUTH")
	if errWrite := writeTestRESPCommand(conn, "AUTH", managementPassword); errWrite != nil {
		t.Fatal(errWrite)
	}
	if msg, errRead := readTestRESPSimpleString(reader); errRead != nil || msg != "OK" {
		t.Fatalf("AUTH after 11 unauthenticated commands: %q, %v; want OK (no ban)", msg, errRead)
	}

	wrong, wrongReader := dial()
	for i := 0; i < 5; i++ {
		expectError(wrong, wrongReader, "ERR invalid management key", "AUTH", "a-wrong-password")
	}
	expectError(wrong, wrongReader, "ERR IP banned due to too many failed attempts", "AUTH", managementPassword)
}
