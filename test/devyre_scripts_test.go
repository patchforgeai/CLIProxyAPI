package test

// devyre: regression tests for the Windows PowerShell scripts in devyre/scripts.
// They run the real scripts with powershell.exe against a fake `tailscale status
// --json` file and a fake management API, and read back what the scripts wrote.
// The tests skip where Windows PowerShell is not available. Every name, IP and
// login below is fake: a CGNAT tailnet range, example.com logins and an example
// tailnet name.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const (
	devyreScriptKey    = "fake-management-key-for-script-tests"
	devyreOwnerLogin   = "owner@example.com"
	devyreTailnetFQDN  = "devbox.example-tailnet.ts.net"
	devyreTailnetShort = "devbox"
)

var devyreScriptsDir = filepath.Join("..", "devyre", "scripts")

// devyreNode is one entry of `tailscale status --json` (Self or a Peer).
type devyreNode struct {
	ID           string   `json:"ID"`
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	UserID       int64    `json:"UserID"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Tags         []string `json:"Tags,omitempty"`
	Created      string   `json:"Created"`
	Online       bool     `json:"Online"`
}

type devyreUser struct {
	ID        int64  `json:"ID"`
	LoginName string `json:"LoginName"`
}

type devyreStatus struct {
	BackendState string                `json:"BackendState"`
	Self         devyreNode            `json:"Self"`
	Peer         map[string]devyreNode `json:"Peer"`
	User         map[string]devyreUser `json:"User"`
}

// devyreTailnet is a single-owner tailnet like the deployment's: this PC, an
// iPhone (whose host name is "localhost"), a laptop, two automation hosts that
// share the owner's login, a tagged node and another user's PC.
func devyreTailnet() devyreStatus {
	owner := int64(1001)
	node := func(id, host, label, os string, last string, extra func(*devyreNode)) devyreNode {
		n := devyreNode{
			ID:           id,
			HostName:     host,
			DNSName:      label + ".example-tailnet.ts.net.",
			OS:           os,
			UserID:       owner,
			TailscaleIPs: []string{"100.64.0." + last, "fd7a:115c:a1e0::" + last},
			Created:      "2024-01-02T03:04:05Z",
			Online:       true,
		}
		if extra != nil {
			extra(&n)
		}
		return n
	}
	return devyreStatus{
		BackendState: "Running",
		Self:         node("nSELF0000CNTRL", "DEVBOX", "devbox", "windows", "10", nil),
		Peer: map[string]devyreNode{
			"nodekey:phone":  node("nPHONE000CNTRL", "localhost", "iphone-a", "iOS", "11", nil),
			"nodekey:laptop": node("nLAPTOP00CNTRL", "LAPTOP", "laptop", "windows", "12", nil),
			"nodekey:ci":     node("nCIRUN000CNTRL", "ci-runner", "ci-runner", "windows", "13", nil),
			"nodekey:bot":    node("nBOTVM000CNTRL", "bot-vm", "bot-vm", "linux", "14", nil),
			"nodekey:tagged": node("nTAGGED00CNTRL", "build-box", "build-box", "linux", "15", func(n *devyreNode) { n.Tags = []string{"tag:ci"} }),
			"nodekey:friend": node("nFRIEND00CNTRL", "friend-pc", "friend-pc", "windows", "16", func(n *devyreNode) { n.UserID = 2002 }),
		},
		User: map[string]devyreUser{
			"1001": {ID: 1001, LoginName: devyreOwnerLogin},
			"2002": {ID: 2002, LoginName: "friend@example.com"},
		},
	}
}

// devyreFakeManagementAPI answers the management API requests tailnet-trust.ps1
// makes: the session probe, the management section and the tailnet-auth block.
type devyreFakeManagementAPI struct {
	mu      sync.Mutex
	block   map[string]any // nil: the block does not exist (404)
	section map[string]any // every management key except tailnet-auth
	puts    []map[string]any
}

func newDevyreFakeManagementAPI(t *testing.T, block map[string]any) (*devyreFakeManagementAPI, string) {
	t.Helper()
	api := &devyreFakeManagementAPI{
		block: block,
		section: map[string]any{
			"allow-remote":  true,
			"secret-key":    "$2a$10$fakehashfakehashfakehashfakehashfakehashfakehashfakeh",
			"disable-panel": false,
		},
	}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return api, server.URL
}

func (api *devyreFakeManagementAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if r.Header.Get("Authorization") != "Bearer "+devyreScriptKey {
		reply(http.StatusUnauthorized, map[string]string{"error": "invalid management key"})
		return
	}
	const blockPath = "/v8/management/config/management/tailnet-auth"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v8/management/auth/session":
		reply(http.StatusOK, map[string]any{"authenticated": true, "method": "key", "login": "", "device": ""})
	case r.Method == http.MethodGet && r.URL.Path == "/v8/management/config/management":
		section := map[string]any{}
		for key, value := range api.section {
			section[key] = value
		}
		if api.block != nil {
			section["tailnet-auth"] = api.block
		}
		reply(http.StatusOK, section)
	case r.Method == http.MethodGet && r.URL.Path == blockPath:
		if api.block == nil {
			reply(http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		reply(http.StatusOK, api.block)
	case r.Method == http.MethodPut && r.URL.Path == blockPath:
		raw, errRead := io.ReadAll(r.Body)
		var block map[string]any
		if errRead != nil || json.Unmarshal(raw, &block) != nil {
			reply(http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		api.block = block
		api.puts = append(api.puts, block)
		reply(http.StatusOK, map[string]string{"status": "ok"})
	default:
		reply(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (api *devyreFakeManagementAPI) writes() []map[string]any {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]map[string]any(nil), api.puts...)
}

// devyrePowerShell returns powershell.exe, or skips the test.
func devyrePowerShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the devyre scripts target Windows PowerShell 5.1")
	}
	path, errLook := exec.LookPath("powershell.exe")
	if errLook != nil {
		t.Skip("powershell.exe is not available")
	}
	return path
}

// devyreScriptEnv is the current environment with USERPROFILE, TEMP and TMP moved
// into dir and PATH reduced to binDir plus the Windows system folders, so a
// script can neither touch the real CPA home nor reach a real docker or
// tailscale.
func devyreScriptEnv(t *testing.T, dir, binDir string) []string {
	t.Helper()
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	system32 := filepath.Join(systemRoot, "System32")
	path := strings.Join([]string{binDir, system32, systemRoot, filepath.Join(system32, "WindowsPowerShell", "v1.0")}, string(os.PathListSeparator))
	override := map[string]string{"USERPROFILE": dir, "TEMP": dir, "TMP": dir, "PATH": path}
	env := make([]string, 0, len(os.Environ())+len(override))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := override[strings.ToUpper(name)]; replaced {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range override {
		env = append(env, name+"="+value)
	}
	return env
}

type devyreScriptRun struct {
	exitCode int
	output   string
}

// runDevyreScript runs one script with -File and returns its exit code and its
// combined output.
func runDevyreScript(t *testing.T, env []string, script string, args ...string) devyreScriptRun {
	t.Helper()
	command := append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(devyreScriptsDir, script)}, args...)
	cmd := exec.Command(devyrePowerShell(t), command...)
	cmd.Env = env
	output, errRun := cmd.CombinedOutput()
	run := devyreScriptRun{output: string(output)}
	var exitErr *exec.ExitError
	switch {
	case errRun == nil:
	case errors.As(errRun, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", script, errRun)
	}
	return run
}

// devyreTrustFixture prepares one tailnet-trust.ps1 run: a status file, a key
// file, a fake management API and a sandboxed environment.
type devyreTrustFixture struct {
	t      *testing.T
	api    *devyreFakeManagementAPI
	base   string
	env    []string
	status string
	key    string
}

func newDevyreTrustFixture(t *testing.T, status devyreStatus, block map[string]any) *devyreTrustFixture {
	t.Helper()
	devyrePowerShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if errMkdir := os.MkdirAll(binDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	raw, errMarshal := json.Marshal(status)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	statusFile := filepath.Join(dir, "status.json")
	keyFile := filepath.Join(dir, "management-key.txt")
	if errWrite := os.WriteFile(statusFile, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := os.WriteFile(keyFile, []byte(devyreScriptKey), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	api, base := newDevyreFakeManagementAPI(t, block)
	return &devyreTrustFixture{t: t, api: api, base: base, env: devyreScriptEnv(t, dir, binDir), status: statusFile, key: keyFile}
}

func (f *devyreTrustFixture) run(args ...string) devyreScriptRun {
	f.t.Helper()
	all := append([]string{"-StatusFile", f.status, "-KeyFile", f.key, "-ApiBase", f.base}, args...)
	return runDevyreScript(f.t, f.env, "tailnet-trust.ps1", all...)
}

// written returns the single block the script wrote, failing unless it wrote
// exactly one.
func (f *devyreTrustFixture) written(run devyreScriptRun) map[string]any {
	f.t.Helper()
	writes := f.api.writes()
	if run.exitCode != 0 || len(writes) != 1 {
		f.t.Fatalf("exit code %d with %d writes, want 0 and 1; output:\n%s", run.exitCode, len(writes), run.output)
	}
	return writes[0]
}

func devyreStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

// The tailnet names in allowed-hosts carry tailscale serve's port: the server
// trusts a tailnet name only together with that port, so a page that gets the
// name resolved to 127.0.0.1 and reaches the loopback publish gets nothing.
func TestDevyreTailnetTrust_WritesTailnetHostsWithTheServePort(t *testing.T) {
	f := newDevyreTrustFixture(t, devyreTailnet(), nil)
	block := f.written(f.run("-Include", devyreTailnetShort))
	want := []string{devyreTailnetFQDN + ":8318", devyreTailnetShort + ":8318", "localhost", "127.0.0.1"}
	if got := devyreStrings(block["allowed-hosts"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed-hosts = %v, want %v", got, want)
	}
	if got := devyreStrings(block["allowed-logins"]); !reflect.DeepEqual(got, []string{devyreOwnerLogin}) {
		t.Fatalf("allowed-logins = %v", got)
	}

	// A re-run without -Include keeps the selection and rewrites bare names, as
	// an older tailnet-trust.ps1 wrote them, with the port.
	old := map[string]any{
		"enabled":         true,
		"allowed-logins":  []any{devyreOwnerLogin},
		"allowed-devices": []any{"100.64.0.10", "fd7a:115c:a1e0::10"},
		"allowed-hosts":   []any{devyreTailnetFQDN, devyreTailnetShort, "localhost", "127.0.0.1"},
		"allow-local":     true,
		"proxy-api":       true,
	}
	g := newDevyreTrustFixture(t, devyreTailnet(), old)
	rerun := g.written(g.run())
	if got := devyreStrings(rerun["allowed-hosts"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("re-run allowed-hosts = %v, want %v", got, want)
	}
	if got := devyreStrings(rerun["allowed-devices"]); !reflect.DeepEqual(got, []string{"100.64.0.10", "fd7a:115c:a1e0::10"}) {
		t.Fatalf("re-run allowed-devices = %v", got)
	}
}
