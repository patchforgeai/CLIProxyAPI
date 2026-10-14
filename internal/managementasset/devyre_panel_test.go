package managementasset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The Devyre fork builds the panel into the binary (devyre_panel.go). These tests pin the
// vendored file to its version record and the embedded bytes to the file, check that the
// devyre_nopanel tag leaves both out, and prove that nothing is downloaded. They pass in both
// builds: go test ./internal/managementasset/ and the same with -tags devyre_nopanel.

const (
	devyrePanelHTMLFile   = "devyre_panel/management.html"
	devyrePanelRecordFile = "devyre_panel/panel.json"
	devyrePanelReleaseTag = "https://github.com/patchforgeai/Cli-Proxy-API-Management-Center/releases/tag/"
)

var (
	// update-panel.ps1 records a release tag, or `git describe --tags` of a clean local build.
	devyrePanelVersionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+-devyre\.\d+(-\d+-g[0-9a-f]{7,40})?$`)
	devyrePanelLocalSource    = regexp.MustCompile(`^local build of [0-9a-f]{40}$`)
)

func readDevyrePanelFiles(t *testing.T) ([]byte, panelRecord) {
	t.Helper()
	html, errHTML := os.ReadFile(filepath.FromSlash(devyrePanelHTMLFile))
	if errHTML != nil {
		t.Fatalf("read the vendored panel: %v", errHTML)
	}
	raw, errRecord := os.ReadFile(filepath.FromSlash(devyrePanelRecordFile))
	if errRecord != nil {
		t.Fatalf("read the panel record: %v", errRecord)
	}
	var record panelRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&record); errDecode != nil {
		t.Fatalf("decode %s: %v", devyrePanelRecordFile, errDecode)
	}
	return html, record
}

// devyreRecordRequests replaces http.DefaultTransport, which the updater's clients use, with
// one that records every request and refuses it, so a fetch fails the test instead of
// reaching GitHub.
func devyreRecordRequests(t *testing.T) func() []string {
	t.Helper()
	var (
		mu        sync.Mutex
		requested []string
	)
	original := http.DefaultTransport
	http.DefaultTransport = devyreRoundTripFunc(func(req *http.Request) (*http.Response, error) {
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

func TestDevyrePanel_RecordMatchesTheFile(t *testing.T) {
	html, record := readDevyrePanelFiles(t)
	sum := sha256.Sum256(html)
	if got := hex.EncodeToString(sum[:]); got != record.SHA256 {
		t.Fatalf("%s hashes to %s but %s records %s; refresh both with devyre/scripts/update-panel.ps1, never by hand",
			devyrePanelHTMLFile, got, devyrePanelRecordFile, record.SHA256)
	}
	if !devyrePanelVersionPattern.MatchString(record.Version) {
		t.Errorf("panel version %q is not a panel fork tag or a clean build of one (v<upstream>-devyre.<n>)", record.Version)
	}
	if record.Source != devyrePanelReleaseTag+record.Version && !devyrePanelLocalSource.MatchString(record.Source) {
		t.Errorf("panel source %q is neither the release %s nor \"local build of <commit>\"", record.Source, devyrePanelReleaseTag+record.Version)
	}
	if len(html) == 0 || len(html) > maxAssetDownloadSize {
		t.Fatalf("panel size = %d bytes, want 1 to %d", len(html), maxAssetDownloadSize)
	}
	if !bytes.HasPrefix(bytes.ToLower(html[:min(len(html), 64)]), []byte("<!doctype html>")) {
		t.Errorf("the vendored panel does not start with <!doctype html>: %q", html[:min(len(html), 64)])
	}
}

func TestDevyrePanel_EmbeddedBytesAreTheFile(t *testing.T) {
	panel := Panel()
	if !devyrePanelIncluded {
		if panel != nil || len(devyrePanelHTML) != 0 || len(devyrePanelRecord) != 0 {
			t.Fatal("a devyre_nopanel build carries panel bytes")
		}
		return
	}
	if panel == nil {
		t.Fatal("Panel() = nil in a build that embeds the panel")
	}
	html, record := readDevyrePanelFiles(t)
	if !bytes.Equal(panel.HTML, html) {
		t.Fatalf("embedded panel (%d bytes) differs from %s (%d bytes)", len(panel.HTML), devyrePanelHTMLFile, len(html))
	}
	if panel.SHA256 != record.SHA256 || panel.Version != record.Version {
		t.Fatalf("Panel() = version %q sha256 %s, want %q %s", panel.Version, panel.SHA256, record.Version, record.SHA256)
	}
	if Panel() != panel {
		t.Fatal("Panel() hashed the panel again; it is computed once")
	}
}

// The default build embeds the panel and its record; devyre_nopanel embeds nothing. go list
// answers from the build constraints and //go:embed lines, offline and without building.
func TestDevyrePanel_NoPanelTagLeavesTheFilesOut(t *testing.T) {
	goTool, errLook := exec.LookPath("go")
	if errLook != nil {
		t.Skip("the go command is not on PATH")
	}
	embedFiles := func(tags string) string {
		t.Helper()
		cmd := exec.Command(goTool, "list", "-tags="+tags, "-f", `{{join .EmbedFiles ","}}`, ".")
		env := make([]string, 0, len(os.Environ())+2)
		for _, entry := range os.Environ() {
			if name, _, _ := strings.Cut(entry, "="); !strings.EqualFold(name, "GOFLAGS") && !strings.EqualFold(name, "GOPROXY") {
				env = append(env, entry)
			}
		}
		cmd.Env = append(env, "GOFLAGS=-mod=readonly", "GOPROXY=off")
		output, errRun := cmd.CombinedOutput()
		if errRun != nil {
			t.Fatalf("go list -tags=%q: %v\n%s", tags, errRun, output)
		}
		return strings.TrimSpace(string(output))
	}
	if got, want := embedFiles(""), devyrePanelHTMLFile+","+devyrePanelRecordFile; got != want {
		t.Errorf("default build embeds %q, want %q", got, want)
	}
	if got := embedFiles("devyre_nopanel"); got != "" {
		t.Errorf("devyre_nopanel build embeds %q, want nothing", got)
	}
}

func TestDevyrePanel_NothingIsDownloaded(t *testing.T) {
	requests := devyreRecordRequests(t)
	SetCurrentConfig(&config.Config{})
	t.Cleanup(func() { SetCurrentConfig(nil) })

	staticDir := t.TempDir()
	if EnsureLatestManagementHTML(t.Context(), staticDir, "", devyrePanelRepository) {
		t.Error("EnsureLatestManagementHTML() = true with no panel file on disk")
	}
	if entries, errRead := os.ReadDir(staticDir); errRead != nil || len(entries) != 0 {
		t.Errorf("static dir after EnsureLatestManagementHTML: %d entries (%v), want it untouched", len(entries), errRead)
	}
	lastUpdateCheckMu.Lock()
	synced := !lastUpdateCheckTime.IsZero()
	lastUpdateCheckMu.Unlock()
	if synced {
		t.Error("EnsureLatestManagementHTML ran its sync")
	}

	// StartAutoUpdater returns before it records the config path or starts its goroutine.
	StartAutoUpdater(t.Context(), filepath.Join(t.TempDir(), "config.yaml"))
	if path := schedulerConfigPath.Load(); path != nil {
		t.Errorf("StartAutoUpdater recorded config path %v, so an updater would run", path)
	}
	started := true
	schedulerOnce.Do(func() { started = false })
	if started {
		t.Error("StartAutoUpdater started the updater goroutine")
	}

	if got := requests(); len(got) != 0 {
		t.Fatalf("network requests = %v, want none", got)
	}
}
