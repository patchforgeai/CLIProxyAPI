package cliproxy

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestDevyreTheNetworkIsBackWhenATokenEndpointTakesAConnection(t *testing.T) {
	targets := devyreNetworkTargets("")
	if len(targets) != 2 || targets[0] != "platform.claude.com:443" || targets[1] != "auth.openai.com:443" {
		t.Fatalf("targets = %v, want the token endpoints", targets)
	}
	if got := devyreNetworkTargets("http://127.0.0.1:8080"); len(got) != 1 || got[0] != "127.0.0.1:8080" {
		t.Fatalf("with a proxy: %v, want the proxy", got)
	}
	if got := devyreNetworkTargets("socks5://proxy.example"); len(got) != 1 || got[0] != "proxy.example:1080" {
		t.Fatalf("with a socks proxy: %v", got)
	}
	if got := devyreNetworkTargets("direct"); len(got) != 2 {
		t.Fatalf("direct: %v, want the token endpoints", got)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	dialer := &net.Dialer{}
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route to host") }
	if devyreNetworkAnswers(context.Background(), []string{"a:443"}, refuse) {
		t.Fatal("a refused dial reads as the network back")
	}
	if !devyreNetworkAnswers(context.Background(), []string{"a:443", listener.Addr().String()}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "a:443" {
			return nil, errors.New("no such host")
		}
		return dialer.DialContext(ctx, network, address)
	}) {
		t.Fatal("an answering address reads as no network")
	}
}
