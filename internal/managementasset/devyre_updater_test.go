package managementasset

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The Devyre fork serves the management panel from its own GitHub releases.
// These tests fail if an upstream sync silently restores the stock panel source.

const (
	devyrePanelRepository = "https://github.com/Devyre/Cli-Proxy-API-Management-Center"
	devyrePanelReleaseAPI = "https://api.github.com/repos/Devyre/Cli-Proxy-API-Management-Center/releases/latest"
	devyrePanelFallback   = "https://github.com/Devyre/Cli-Proxy-API-Management-Center/releases/latest/download/management.html"
)

type devyreRoundTripFunc func(*http.Request) (*http.Response, error)

func (f devyreRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDevyrePanelSource_DefaultsPointAtFork(t *testing.T) {
	if config.DefaultPanelGitHubRepository != devyrePanelRepository {
		t.Fatalf("DefaultPanelGitHubRepository = %q, want %q", config.DefaultPanelGitHubRepository, devyrePanelRepository)
	}
	if defaultManagementReleaseURL != devyrePanelReleaseAPI {
		t.Fatalf("defaultManagementReleaseURL = %q, want %q", defaultManagementReleaseURL, devyrePanelReleaseAPI)
	}
	if defaultManagementFallbackURL != devyrePanelFallback {
		t.Fatalf("defaultManagementFallbackURL = %q, want %q", defaultManagementFallbackURL, devyrePanelFallback)
	}

	for _, repo := range []string{"", "   ", config.DefaultPanelGitHubRepository, devyrePanelRepository + "/", devyrePanelRepository + ".git", "not a url"} {
		if got := resolveReleaseURL(repo); got != devyrePanelReleaseAPI {
			t.Errorf("resolveReleaseURL(%q) = %q, want %q", repo, got, devyrePanelReleaseAPI)
		}
	}
}

func TestDevyrePanelSource_FallbackNeverUsesStockPanel(t *testing.T) {
	parsed, errParse := url.Parse(defaultManagementFallbackURL)
	if errParse != nil {
		t.Fatalf("parse fallback URL: %v", errParse)
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" {
		t.Fatalf("fallback URL %q must be an https github.com release download", defaultManagementFallbackURL)
	}
	if !strings.HasPrefix(parsed.Path, "/Devyre/Cli-Proxy-API-Management-Center/releases/") || !strings.HasSuffix(parsed.Path, "/"+managementAssetName) {
		t.Fatalf("fallback URL %q must download %s from the Devyre panel releases", defaultManagementFallbackURL, managementAssetName)
	}
	for _, stock := range []string{"router-for", "cpamc."} {
		if strings.Contains(strings.ToLower(defaultManagementFallbackURL), stock) {
			t.Fatalf("fallback URL %q points at the stock panel (%q)", defaultManagementFallbackURL, stock)
		}
	}
}

func TestDevyrePanelSource_FallbackDownloadsForkAsset(t *testing.T) {
	var (
		mu        sync.Mutex
		requested []string
	)
	client := &http.Client{Transport: devyreRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requested = append(requested, req.URL.String())
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html>devyre panel</html>")),
			Request:    req,
		}, nil
	})}

	localPath := filepath.Join(t.TempDir(), managementAssetName)
	if !ensureFallbackManagementHTML(t.Context(), client, localPath) {
		t.Fatal("ensureFallbackManagementHTML() = false, want true")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requested) != 1 || requested[0] != devyrePanelFallback {
		t.Fatalf("fallback requests = %v, want exactly [%s]", requested, devyrePanelFallback)
	}
	data, errRead := os.ReadFile(localPath)
	if errRead != nil {
		t.Fatalf("read fallback asset: %v", errRead)
	}
	if string(data) != "<html>devyre panel</html>" {
		t.Fatalf("fallback asset = %q, want the downloaded body", data)
	}
}
