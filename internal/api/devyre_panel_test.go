package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
)

// devyre: /management.html comes from the panel built into the binary
// (devyre_panel.go), never from a download. Both builds run these tests:
//
//	go test ./internal/api/ -run DevyrePanel                         (the embedded panel)
//	go test -tags devyre_nopanel ./internal/api/ -run DevyrePanel    (404)
//
// internal/managementasset pins which build embeds the panel; here the route
// follows managementasset.Panel().

const devyrePanelFileOnDisk = "../managementasset/devyre_panel/management.html"

// devyreRefuseNetwork replaces http.DefaultTransport, which the panel
// downloader's clients use, with one that records and refuses every request
// that leaves this machine. Loopback requests, which only another test's
// leftover goroutine against its own httptest server could make, pass through.
func devyreRefuseNetwork(t *testing.T) func() []string {
	t.Helper()
	var (
		mu        sync.Mutex
		requested []string
	)
	original := http.DefaultTransport
	http.DefaultTransport = devyrePanelRoundTrip(func(req *http.Request) (*http.Response, error) {
		if ip := net.ParseIP(req.URL.Hostname()); ip != nil && ip.IsLoopback() {
			return original.RoundTrip(req)
		}
		mu.Lock()
		requested = append(requested, req.Method+" "+req.URL.String())
		mu.Unlock()
		return nil, errors.New("devyre test: network access refused")
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requested...)
	}
}

type devyrePanelRoundTrip func(*http.Request) (*http.Response, error)

func (f devyrePanelRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func devyrePanelRequest(server *Server, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	for name, values := range header {
		req.Header[name] = values
	}
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	return rec
}

// devyreFilesUnder lists the regular files below dir.
func devyreFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	errWalk := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, errEntry error) error {
		if errEntry != nil {
			return errEntry
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk %s: %v", dir, errWalk)
	}
	return files
}

func TestDevyrePanel_ServedFromTheBinaryWithoutNetwork(t *testing.T) {
	t.Setenv("MANAGEMENT_STATIC_PATH", "")
	writable := t.TempDir()
	t.Setenv("WRITABLE_PATH", writable) // upstream's panel file would land in <writable>/static
	requests := devyreRefuseNetwork(t)
	server := newTestServer(t)

	rec := devyrePanelRequest(server, nil)
	if got := requests(); len(got) != 0 {
		t.Fatalf("network requests = %v, want none", got)
	}
	if panel := managementasset.Panel(); panel == nil {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("devyre_nopanel build: status = %d, want 404", rec.Code)
		}
	} else {
		want, errRead := os.ReadFile(filepath.FromSlash(devyrePanelFileOnDisk))
		if errRead != nil {
			t.Fatalf("read the vendored panel: %v", errRead)
		}
		sum := sha256.Sum256(want)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("status = %d, body %d bytes; want 200 with the vendored panel (%d bytes)", rec.Code, rec.Body.Len(), len(want))
		}
		for name, value := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           "no-cache",
			"ETag":                    etag,
			"Content-Security-Policy": "frame-ancestors 'none'",
			"X-Frame-Options":         "DENY",
		} {
			if got := rec.Header().Get(name); got != value {
				t.Errorf("%s = %q, want %q", name, got, value)
			}
		}

		revalidated := devyrePanelRequest(server, http.Header{"If-None-Match": {etag}})
		if revalidated.Code != http.StatusNotModified || revalidated.Body.Len() != 0 {
			t.Errorf("revalidation: status = %d, body %d bytes; want 304 and no body", revalidated.Code, revalidated.Body.Len())
		}
	}

	if got := requests(); len(got) != 0 {
		t.Fatalf("network requests = %v, want none", got)
	}
	if files := devyreFilesUnder(t, writable); len(files) != 0 {
		t.Fatalf("files written = %v, want none (nothing is downloaded or persisted)", files)
	}
}

// MANAGEMENT_STATIC_PATH is upstream's explicit relocation of the panel file:
// a file there is served as it is, and a missing one is never downloaded.
func TestDevyrePanel_StaticPathOverrideAndDisabledPanel(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	requests := devyreRefuseNetwork(t)
	server := newTestServer(t)

	rec := devyrePanelRequest(server, nil)
	if got := requests(); len(got) != 0 {
		t.Fatalf("network requests = %v, want none", got)
	}
	if panel := managementasset.Panel(); panel == nil {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("no file and no built-in panel: status = %d, want 404", rec.Code)
		}
	} else if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), panel.HTML) {
		t.Fatalf("no file: status = %d, body %d bytes; want the built-in panel", rec.Code, rec.Body.Len())
	}
	if files := devyreFilesUnder(t, staticDir); len(files) != 0 {
		t.Fatalf("files written = %v, want none", files)
	}

	const local = "<html>local panel build</html>"
	if errWrite := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte(local), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if rec = devyrePanelRequest(server, nil); rec.Code != http.StatusOK || rec.Body.String() != local {
		t.Fatalf("override file: status = %d body = %q, want 200 %q", rec.Code, rec.Body.String(), local)
	}

	server.cfg.RemoteManagement.DisableControlPanel = true
	if rec = devyrePanelRequest(server, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("disable-control-panel: status = %d, want 404", rec.Code)
	}

	if got := requests(); len(got) != 0 {
		t.Fatalf("network requests = %v, want none", got)
	}
}
