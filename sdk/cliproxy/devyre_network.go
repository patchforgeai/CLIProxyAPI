package cliproxy

// devyre: the refresh loop's look at the network after a wake (sdk/cliproxy/auth/devyre_wake.go;
// devyre/README.md, "A refresh that failed in passing"). A connection the token endpoints take,
// or the configured proxy, means the network is back: the refreshes held for it go out at once
// instead of after the whole wait.

import (
	"context"
	"net"
	"net/url"
	"strings"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// devyreNetworkTargets are the addresses whose answer says the network is back: the configured
// proxy when there is one (the refreshes go through it), else the token endpoints.
func devyreNetworkTargets(proxyURL string) []string {
	if target := proxyAddress(proxyURL); target != "" {
		return []string{target}
	}
	var targets []string
	for _, endpoint := range []string{claudeauth.RefreshTokenURL, codexauth.TokenURL} {
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Hostname() != "" {
			targets = append(targets, net.JoinHostPort(parsed.Hostname(), "443"))
		}
	}
	return targets
}

func proxyAddress(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || strings.EqualFold(proxyURL, "direct") || strings.EqualFold(proxyURL, "none") {
		return ""
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port)
}

// devyreNetworkAnswers reports whether any of targets takes a connection before ctx ends.
func devyreNetworkAnswers(ctx context.Context, targets []string, dial func(context.Context, string, string) (net.Conn, error)) bool {
	answered := make(chan bool, len(targets))
	for _, target := range targets {
		go func() {
			conn, err := dial(ctx, "tcp", target)
			if err == nil {
				_ = conn.Close()
			}
			answered <- err == nil
		}()
	}
	for range targets {
		if <-answered {
			return true
		}
	}
	return false
}

// devyreLookAtTheNetwork gives the refresh loop its look at the network.
func (s *Service) devyreLookAtTheNetwork() {
	if s == nil || s.coreManager == nil || s.cfg == nil {
		return
	}
	targets := devyreNetworkTargets(s.cfg.ProxyURL)
	dialer := &net.Dialer{}
	s.coreManager.SetNetworkProbe(func(ctx context.Context) bool {
		return devyreNetworkAnswers(ctx, targets, dialer.DialContext)
	})
}
