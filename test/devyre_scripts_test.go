package test

// devyre: regression tests for the Windows PowerShell scripts in devyre/scripts.
// They run the real scripts with powershell.exe against a fake `tailscale status
// --json` file and a fake management API, and read back what the scripts wrote.
// The tests skip where Windows PowerShell is not available. Every name, IP and
// login below is fake: a CGNAT tailnet range, example.com logins and an example
// tailnet name.

import (
	"crypto/sha256"
	"encoding/hex"
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
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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

// current returns the block the fake API holds after a successful run, whether
// the run wrote it or found it already in place.
func (f *devyreTrustFixture) current(run devyreScriptRun) map[string]any {
	f.t.Helper()
	if run.exitCode != 0 {
		f.t.Fatalf("exit code %d, want 0; output:\n%s", run.exitCode, run.output)
	}
	f.api.mu.Lock()
	defer f.api.mu.Unlock()
	return f.api.block
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

// devyreLiveBlock is a tailnet-auth block that allows the devices with these
// last IP octets (IPv4 and IPv6).
func devyreLiveBlock(lastOctets ...string) map[string]any {
	devices := []any{}
	for _, last := range lastOctets {
		devices = append(devices, "100.64.0."+last, "fd7a:115c:a1e0::"+last)
	}
	return map[string]any{
		"enabled":         true,
		"allowed-logins":  []any{devyreOwnerLogin},
		"allowed-devices": devices,
		"allowed-hosts":   []any{devyreTailnetFQDN + ":8318", devyreTailnetShort + ":8318", "localhost", "127.0.0.1"},
		"allow-local":     true,
		"proxy-api":       true,
	}
}

// devyreDevicesOf returns the sorted allowed-devices of a written block.
func devyreDevicesOf(block map[string]any) []string {
	devices := devyreStrings(block["allowed-devices"])
	sort.Strings(devices)
	return devices
}

func devyreWantDevices(lastOctets ...string) []string {
	var want []string
	for _, last := range lastOctets {
		want = append(want, "100.64.0."+last, "fd7a:115c:a1e0::"+last)
	}
	sort.Strings(want)
	return want
}

// devyreRename makes a peer report a new host name. Its MagicDNS name follows,
// deduplicated with a suffix when another node holds that name, as the control
// plane does unless the machine name is pinned in the admin console.
func devyreRename(status *devyreStatus, peer, hostName, label string) {
	node := status.Peer[peer]
	node.HostName = hostName
	node.DNSName = label + ".example-tailnet.ts.net."
	status.Peer[peer] = node
}

// A compromised automation host that renames itself to fit the owner's wildcard
// or exact names must not be selected. Before the fix, -Include matched the
// self-reported host name and wildcards added new devices, so both renamed
// automation hosts below were written into allowed-devices.
func TestDevyreTailnetTrust_RenamedAutomationHostsAreNotSelected(t *testing.T) {
	status := devyreTailnet()
	devyreRename(&status, "nodekey:ci", "iphone-17", "iphone-17") // fits iphone*
	devyreRename(&status, "nodekey:bot", "LAPTOP", "laptop-1")    // the laptop's host name, deduplicated label
	f := newDevyreTrustFixture(t, status, devyreLiveBlock("10", "11"))

	run := f.run("-Include", devyreTailnetShort+",iphone*,laptop", "-Force")
	block := f.written(run)
	if got, want := devyreDevicesOf(block), devyreWantDevices("10", "11", "12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed-devices = %v, want this PC, the allowed iPhone and the laptop named exactly (%v); output:\n%s", got, want, run.output)
	}
	if !strings.Contains(run.output, "new device matched only by the wildcard iphone*") {
		t.Fatalf("output does not explain why iphone-17 was left out:\n%s", run.output)
	}

	// An exact name matches the unique MagicDNS name only, never a host name
	// another node copied.
	status = devyreTailnet()
	devyreRename(&status, "nodekey:bot", "DEVBOX", "devbox-1")
	g := newDevyreTrustFixture(t, status, nil)
	block = g.written(g.run("-Include", devyreTailnetShort))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10"); !reflect.DeepEqual(got, want) {
		t.Fatalf("-Include %s selected %v, want only this PC %v", devyreTailnetShort, got, want)
	}
}

// Wildcards keep devices that are already allowed (and this PC) but never add
// a new one; -Include * on a first run therefore selects only this PC.
func TestDevyreTailnetTrust_WildcardsNeverAddNewDevices(t *testing.T) {
	f := newDevyreTrustFixture(t, devyreTailnet(), devyreLiveBlock("10", "11"))
	run := f.run("-Include", "*")
	if got, want := devyreDevicesOf(f.current(run)), devyreWantDevices("10", "11"); !reflect.DeepEqual(got, want) || len(f.api.writes()) != 0 {
		t.Fatalf("-Include * selected %v with %d writes, want the already allowed devices %v unchanged; output:\n%s", got, len(f.api.writes()), want, run.output)
	}
	if !strings.Contains(run.output, "new device matched only by the wildcard *") {
		t.Fatalf("output does not say why the new devices were left out:\n%s", run.output)
	}

	g := newDevyreTrustFixture(t, devyreTailnet(), nil)
	block := g.written(g.run("-Include", "*"))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10"); !reflect.DeepEqual(got, want) {
		t.Fatalf("-Include * on a first run selected %v, want only this PC %v", got, want)
	}

	h := newDevyreTrustFixture(t, devyreTailnet(), devyreLiveBlock("10", "11"))
	block = h.written(h.run("-Include", "*", "-Exclude", "iphone*"))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10"); !reflect.DeepEqual(got, want) {
		t.Fatalf("-Exclude iphone* left %v, want %v", got, want)
	}
}

// A write that adds a new device needs confirmation. A session that cannot ask
// writes nothing; -ShowOnly lists the new device with its node ID, OS and
// registration date.
func TestDevyreTailnetTrust_NewDevicesNeedConfirmation(t *testing.T) {
	f := newDevyreTrustFixture(t, devyreTailnet(), devyreLiveBlock("10"))
	preview := f.run("-Include", devyreTailnetShort+",laptop", "-ShowOnly")
	if preview.exitCode != 0 || len(f.api.writes()) != 0 {
		t.Fatalf("-ShowOnly: exit %d with %d writes, want 0 and none; output:\n%s", preview.exitCode, len(f.api.writes()), preview.output)
	}
	for _, want := range []string{"laptop: node nLAPTOP00CNTRL, OS windows, host name LAPTOP, registered 2024-01-02", "allowed (NEW)"} {
		if !strings.Contains(preview.output, want) {
			t.Fatalf("-ShowOnly output lacks %q:\n%s", want, preview.output)
		}
	}

	refused := f.run("-Include", devyreTailnetShort+",laptop")
	if refused.exitCode != 1 || len(f.api.writes()) != 0 || !strings.Contains(refused.output, "-Force") {
		t.Fatalf("unconfirmed new device: exit %d with %d writes, want 1 and none plus a -Force hint; output:\n%s", refused.exitCode, len(f.api.writes()), refused.output)
	}

	block := f.written(f.run("-Include", devyreTailnetShort+",laptop", "-Force"))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10", "12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("confirmed write = %v, want %v", got, want)
	}

	// Keeping only already allowed devices asks nothing.
	g := newDevyreTrustFixture(t, devyreTailnet(), devyreLiveBlock("10", "11", "12"))
	block = g.written(g.run("-Include", devyreTailnetShort+",laptop"))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10", "12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("re-selection of allowed devices = %v, want %v", got, want)
	}
}

// The automation guard reads the MagicDNS name and the host name, and only an
// exact MagicDNS name in -AllowAutomationName lifts it.
func TestDevyreTailnetTrust_AutomationGuard(t *testing.T) {
	status := devyreTailnet()
	devyreRename(&status, "nodekey:laptop", "ci-runner-7", "laptop") // pinned clean machine name, automation host name
	f := newDevyreTrustFixture(t, status, devyreLiveBlock("10"))
	block := f.current(f.run("-Include", devyreTailnetShort+",laptop,ci-runner", "-AllowAutomationName", "ci-runner-7", "-Force"))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10"); !reflect.DeepEqual(got, want) {
		t.Fatalf("guarded devices were selected: %v, want %v", got, want)
	}

	g := newDevyreTrustFixture(t, devyreTailnet(), devyreLiveBlock("10"))
	block = g.written(g.run("-Include", devyreTailnetShort+",ci-runner", "-AllowAutomationName", "ci-runner", "-Force"))
	if got, want := devyreDevicesOf(block), devyreWantDevices("10", "13"); !reflect.DeepEqual(got, want) {
		t.Fatalf("-AllowAutomationName ci-runner = %v, want %v", got, want)
	}
}

// devyreRenderTemplate renders devyre/deploy/config.template.yaml with fake
// secrets whose names start with prefix.
func devyreRenderTemplate(t *testing.T, prefix string) string {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("..", "devyre", "deploy", "config.template.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	text := string(raw)
	for _, placeholder := range []string{"__MANAGEMENT_KEY__", "__KEY_T3_CODE__", "__KEY_CLAUDE_CODE_CLI__", "__KEY_CODEX_CLI__", "__KEY_OTHER_DEVICES__"} {
		text = strings.ReplaceAll(text, placeholder, prefix+strings.ToLower(strings.Trim(placeholder, "_")))
	}
	return text
}

// devyreServerWritesTailnetAuth lets the real /v8 config write handler store
// block in configPath, exactly as it does when tailnet-trust.ps1 writes it.
func devyreServerWritesTailnetAuth(t *testing.T, configPath, block string) {
	t.Helper()
	cfg, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	gin.SetMode(gin.TestMode)
	handler := management.NewHandler(cfg, configPath, nil)
	router := gin.New()
	router.PUT("/v8/management/config/*path", func(c *gin.Context) {
		c.Set(management.ConfigV8ContextKey, true)
		handler.ConfigV8(c)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v8/management/config/management/tailnet-auth", strings.NewReader(block)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT tailnet-auth: status = %d; body = %s", rec.Code, rec.Body.String())
	}
}

func devyreLoadTailnetAuth(t *testing.T, path string) config.TailnetAuthConfig {
	t.Helper()
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("load %s: %v", path, errLoad)
	}
	return cfg.RemoteManagement.TailnetAuth
}

// new-secrets.ps1 -Rotate re-renders config.yaml from the template. The
// passwordless policy exists only in the runtime config, so the rotation used to
// reset it to the template's empty lists: every device lost keyless access, and
// a tailnet-trust.ps1 re-run without -Include could not restore it. The block is
// now carried over unchanged; -ResetTailnetAuth asks for the template block.
func TestDevyreNewSecrets_RotateKeepsTheTailnetAuthBlock(t *testing.T) {
	devyrePowerShell(t)
	dir := t.TempDir()
	cpaHome := filepath.Join(dir, ".cli-proxy-api")
	binDir := filepath.Join(dir, "bin")
	for _, path := range []string{cpaHome, binDir} {
		if errMkdir := os.MkdirAll(path, 0o700); errMkdir != nil {
			t.Fatal(errMkdir)
		}
	}
	// A docker that records its arguments and fails, so the rotation finds no
	// running container and can never restart a real one.
	calls := filepath.Join(binDir, "docker-calls.txt")
	fakeDocker := "@echo off\r\necho %*>>\"%~dp0docker-calls.txt\"\r\nexit /b 1\r\n"
	if errWrite := os.WriteFile(filepath.Join(binDir, "docker.cmd"), []byte(fakeDocker), 0o700); errWrite != nil {
		t.Fatal(errWrite)
	}
	env := devyreScriptEnv(t, dir, binDir)

	configPath := filepath.Join(cpaHome, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(devyreRenderTemplate(t, "old-fake-")), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	devyreServerWritesTailnetAuth(t, configPath, `{"enabled":true,"allowed-logins":["owner@example.com"],`+
		`"allowed-devices":["100.64.0.10","fd7a:115c:a1e0::10","100.64.0.11"],`+
		`"allowed-hosts":["devbox.example-tailnet.ts.net:8318","devbox:8318","localhost","127.0.0.1"],"allow-local":true,"proxy-api":false}`)
	want := devyreLoadTailnetAuth(t, configPath)
	if !want.Enabled || len(want.AllowedDevices) != 3 || want.ProxyAPI {
		t.Fatalf("the server did not store the block: %+v", want)
	}

	run := runDevyreScript(t, env, "new-secrets.ps1", "-Rotate")
	if run.exitCode != 0 || !strings.Contains(run.output, "Kept management.tailnet-auth") {
		t.Fatalf("-Rotate: exit %d; output:\n%s", run.exitCode, run.output)
	}
	rendered, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	newKey, errKey := os.ReadFile(filepath.Join(cpaHome, "secrets", "management-key.txt"))
	if errKey != nil {
		t.Fatal(errKey)
	}
	if strings.Contains(string(rendered), "old-fake-") || !strings.Contains(string(rendered), strings.TrimSpace(string(newKey))) {
		t.Fatalf("config.yaml was not re-rendered with the new secrets:\n%s", rendered)
	}
	if got := devyreLoadTailnetAuth(t, configPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("tailnet-auth after -Rotate = %+v, want it kept as %+v", got, want)
	}
	backups, _ := filepath.Glob(configPath + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one", backups)
	}
	if got := devyreLoadTailnetAuth(t, backups[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("backup tailnet-auth = %+v, want %+v", got, want)
	}

	// -ResetTailnetAuth starts the policy from the template: on, nothing keyless.
	// The backup name has a one-second resolution, so move the first one away.
	if errRename := os.Rename(backups[0], filepath.Join(dir, "first-backup.yaml")); errRename != nil {
		t.Fatal(errRename)
	}
	reset := runDevyreScript(t, env, "new-secrets.ps1", "-Rotate", "-ResetTailnetAuth")
	if reset.exitCode != 0 || !strings.Contains(reset.output, "reset to the template") {
		t.Fatalf("-Rotate -ResetTailnetAuth: exit %d; output:\n%s", reset.exitCode, reset.output)
	}
	if got := devyreLoadTailnetAuth(t, configPath); !got.Enabled || !got.AllowLocal || !got.ProxyAPI ||
		len(got.AllowedLogins)+len(got.AllowedDevices)+len(got.AllowedHosts) != 0 {
		t.Fatalf("tailnet-auth after -ResetTailnetAuth = %+v, want the template block", got)
	}
	if refused := runDevyreScript(t, env, "new-secrets.ps1", "-ResetTailnetAuth"); refused.exitCode == 0 {
		t.Fatalf("-ResetTailnetAuth without -Rotate was accepted; output:\n%s", refused.output)
	}

	recorded, _ := os.ReadFile(calls)
	if !strings.Contains(string(recorded), "inspect") || strings.Contains(string(recorded), "restart") {
		t.Fatalf("docker calls = %q, want inspections only", recorded)
	}
}

// devyreHelperProbe exercises the cpa-common.ps1 helpers that exposure-check.ps1
// uses to read allowed-hosts. It prints one key=value line per case.
const devyreHelperProbe = `$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
. (Join-Path $args[0] 'cpa-common.ps1')
function Entries([string[]]$Values) { return ,@($Values | ForEach-Object { ConvertTo-CpaHostEntry $_ } | Where-Object { $null -ne $_ }) }
function Out([string]$Key, $Value) { Write-Output ($Key + '=' + $Value) }
foreach ($text in 'machine.example.ts.net:8318', 'MACHINE.Example.ts.net.:8318', 'localhost', '[::1]:8317', '::1', 'x:', 'x:0', 'x:65536', '[::1', '[::1]x') {
  $parsed = ConvertTo-CpaHostEntry $text
  if ($null -eq $parsed) { Out "parse $text" 'invalid' } else { Out "parse $text" "$($parsed.Name) $($parsed.Port)" }
}
$fqdn = 'devbox.example-tailnet.ts.net'
Out 'state ported' (Get-CpaServeHostState (Entries @("${fqdn}:8318", 'localhost')) $fqdn)
Out 'state upper case' (Get-CpaServeHostState (Entries @('DEVBOX.Example-Tailnet.ts.net.:8318')) $fqdn)
Out 'state bare' (Get-CpaServeHostState (Entries @($fqdn, 'devbox', 'localhost')) $fqdn)
Out 'state other port' (Get-CpaServeHostState (Entries @("${fqdn}:8317")) $fqdn)
Out 'state renamed' (Get-CpaServeHostState (Entries @('devbox.old-tailnet.ts.net:8318', 'devbox:8318')) $fqdn)
Out 'empty both' (Test-CpaTailnetAuthEmpty (Entries @()) @())
Out 'empty devices listed' (Test-CpaTailnetAuthEmpty (Entries @()) @('100.64.0.10'))
Out 'empty hosts listed' (Test-CpaTailnetAuthEmpty (Entries @('localhost')) @())
Out 'listed loopback any port' (Test-CpaHostListed (Entries @('127.0.0.1')) '127.0.0.1' 8317)
Out 'listed loopback other port' (Test-CpaHostListed (Entries @('127.0.0.1:9000')) '127.0.0.1' 8317)
Out 'listed bare tailnet name' (Test-CpaHostListed (Entries @($fqdn)) $fqdn 8318 -RequirePort)
`

// exposure-check.ps1 reads allowed-hosts with these helpers. A tailnet name
// listed without serve's port grants nothing, and is reported as written by an
// older tailnet-trust.ps1 ('bare'); a block whose lists are both empty is the
// template's, not a stale name after a rename, as a rotation used to leave it.
func TestDevyreCommon_AllowedHostsHelpers(t *testing.T) {
	devyrePowerShell(t)
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.ps1")
	if errWrite := os.WriteFile(probe, []byte(devyreHelperProbe), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	scripts, errAbs := filepath.Abs(devyreScriptsDir)
	if errAbs != nil {
		t.Fatal(errAbs)
	}
	cmd := exec.Command(devyrePowerShell(t), "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", probe, scripts)
	cmd.Env = devyreScriptEnv(t, dir, dir)
	output, errRun := cmd.CombinedOutput()
	if errRun != nil {
		t.Fatalf("probe failed: %v\n%s", errRun, output)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			got[key] = value
		}
	}
	want := map[string]string{
		"parse machine.example.ts.net:8318":  "machine.example.ts.net 8318",
		"parse MACHINE.Example.ts.net.:8318": "machine.example.ts.net 8318",
		"parse localhost":                    "localhost 0",
		"parse [::1]:8317":                   "::1 8317",
		"parse ::1":                          "::1 0",
		"parse x:":                           "invalid",
		"parse x:0":                          "invalid",
		"parse x:65536":                      "invalid",
		"parse [::1":                         "invalid",
		"parse [::1]x":                       "invalid",
		"state ported":                       "ok",
		"state upper case":                   "ok",
		"state bare":                         "bare",
		"state other port":                   "bare",
		"state renamed":                      "stale",
		"empty both":                         "True",
		"empty devices listed":               "False",
		"empty hosts listed":                 "False",
		"listed loopback any port":           "True",
		"listed loopback other port":         "False",
		"listed bare tailnet name":           "False",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helper results = %v\nwant %v\noutput:\n%s", got, want, output)
	}
}

// devyreFakeGh answers the two gh calls update-panel.ps1 makes, from files next
// to it: `gh api repos/<repo>/releases/latest` prints release.json, and `gh
// release download ... --output <file>` copies asset.html there. Every call is
// appended to gh-calls.txt.
const devyreFakeGh = "@echo off\r\n" +
	"echo %*>>\"%~dp0gh-calls.txt\"\r\n" +
	"if \"%~1\"==\"api\" if \"%~2\"==\"repos/example/panel/releases/latest\" (\r\n" +
	"  type \"%~dp0release.json\"\r\n" +
	"  exit /b 0\r\n" +
	")\r\n" +
	"if \"%~1 %~2\"==\"release download\" if \"%~8\"==\"--output\" (\r\n" +
	"  copy /y \"%~dp0asset.html\" \"%~9\" >nul\r\n" +
	"  exit /b 0\r\n" +
	")\r\n" +
	"echo HTTP 404: Not Found 1>&2\r\n" +
	"exit /b 1\r\n"

// devyreUpdatePanelFixture is a copy of the scripts and the panel folder in a
// temp tree, so update-panel.ps1 rewrites the copy and never the repository.
type devyreUpdatePanelFixture struct {
	t        *testing.T
	env      []string
	binDir   string
	script   string
	panelDir string
}

const devyreSeedPanel = "<!doctype html><html>seed panel</html>"

func newDevyreUpdatePanelFixture(t *testing.T) *devyreUpdatePanelFixture {
	t.Helper()
	devyrePowerShell(t)
	dir := t.TempDir()
	f := &devyreUpdatePanelFixture{
		t:        t,
		binDir:   filepath.Join(dir, "bin"),
		script:   filepath.Join(dir, "repo", "devyre", "scripts", "update-panel.ps1"),
		panelDir: filepath.Join(dir, "repo", "internal", "managementasset", "devyre_panel"),
	}
	for _, path := range []string{f.binDir, filepath.Dir(f.script), f.panelDir} {
		if errMkdir := os.MkdirAll(path, 0o700); errMkdir != nil {
			t.Fatal(errMkdir)
		}
	}
	for _, name := range []string{"update-panel.ps1", "cpa-common.ps1"} {
		raw, errRead := os.ReadFile(filepath.Join(devyreScriptsDir, name))
		if errRead != nil {
			t.Fatal(errRead)
		}
		if errWrite := os.WriteFile(filepath.Join(filepath.Dir(f.script), name), raw, 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	f.write(filepath.Join(f.binDir, "gh.cmd"), devyreFakeGh)
	f.write(filepath.Join(f.panelDir, "management.html"), devyreSeedPanel)
	f.write(filepath.Join(f.panelDir, "panel.json"), `{"version":"v1.0.0-devyre.1","source":"seed","sha256":"`+devyreSha256Hex(devyreSeedPanel)+`"}`)
	f.env = devyreScriptEnv(t, dir, f.binDir)
	return f
}

func devyreSha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (f *devyreUpdatePanelFixture) write(path, content string) {
	f.t.Helper()
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		f.t.Fatal(errWrite)
	}
}

func (f *devyreUpdatePanelFixture) read(name string) string {
	f.t.Helper()
	raw, errRead := os.ReadFile(filepath.Join(f.panelDir, name))
	if errRead != nil {
		f.t.Fatal(errRead)
	}
	return string(raw)
}

// release publishes asset as management.html of release tag, listing digest.
func (f *devyreUpdatePanelFixture) release(tag, asset, digest string) {
	f.t.Helper()
	assetJSON := map[string]any{"name": "management.html", "size": len(asset)}
	if digest != "" {
		assetJSON["digest"] = digest
	}
	raw, errMarshal := json.Marshal(map[string]any{"tag_name": tag, "assets": []any{map[string]any{"name": "notes.txt"}, assetJSON}})
	if errMarshal != nil {
		f.t.Fatal(errMarshal)
	}
	f.write(filepath.Join(f.binDir, "release.json"), string(raw))
	f.write(filepath.Join(f.binDir, "asset.html"), asset)
}

func (f *devyreUpdatePanelFixture) run() devyreScriptRun {
	f.t.Helper()
	cmd := exec.Command(devyrePowerShell(f.t), "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", f.script, "-Repository", "example/panel")
	cmd.Env = f.env
	output, errRun := cmd.CombinedOutput()
	run := devyreScriptRun{output: string(output)}
	var exitErr *exec.ExitError
	switch {
	case errRun == nil:
	case errors.As(errRun, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		f.t.Fatalf("run update-panel.ps1: %v", errRun)
	}
	return run
}

// update-panel.ps1 vendors a release's management.html only when it matches the
// sha256 digest GitHub lists for it, and records version, source and hash next
// to it in the layout internal/managementasset reads (devyre_panel_test.go).
func TestDevyreUpdatePanel_VendorsAVerifiedRelease(t *testing.T) {
	f := newDevyreUpdatePanelFixture(t)
	unchanged := func(name string, run devyreScriptRun, reason string) {
		t.Helper()
		if run.exitCode == 0 || !strings.Contains(run.output, reason) {
			t.Fatalf("%s: exit %d, want a refusal naming %q; output:\n%s", name, run.exitCode, reason, run.output)
		}
		if got := f.read("management.html"); got != devyreSeedPanel {
			t.Fatalf("%s: management.html = %q, want the seed kept", name, got)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(f.panelDir, ".management-*")); len(leftovers) != 0 {
			t.Fatalf("%s: staged files left behind: %v", name, leftovers)
		}
	}

	const panel = "<!DOCTYPE html><html>panel v9</html>\n"
	f.release("v9.9.9-devyre.1", panel, "sha256:"+devyreSha256Hex("something else"))
	unchanged("digest mismatch", f.run(), "Nothing was changed")
	f.release("v9.9.9-devyre.1", panel, "")
	unchanged("no digest", f.run(), "lists no sha256 digest")
	f.release("v9.9.9-devyre.1", "not a page", "sha256:"+devyreSha256Hex("not a page"))
	unchanged("not an HTML page", f.run(), "is not a panel build")

	f.release("v9.9.9-devyre.1", panel, "sha256:"+strings.ToUpper(devyreSha256Hex(panel)))
	run := f.run()
	if run.exitCode != 0 || !strings.Contains(run.output, "Embedded panel: v9.9.9-devyre.1 (was v1.0.0-devyre.1") {
		t.Fatalf("verified release: exit %d; output:\n%s", run.exitCode, run.output)
	}
	if got := f.read("management.html"); got != panel {
		t.Fatalf("management.html = %q, want the release asset byte for byte", got)
	}
	want := "{\n" +
		"  \"version\": \"v9.9.9-devyre.1\",\n" +
		"  \"source\": \"https://github.com/example/panel/releases/tag/v9.9.9-devyre.1\",\n" +
		"  \"sha256\": \"" + devyreSha256Hex(panel) + "\"\n" +
		"}\n"
	if got := f.read("panel.json"); got != want {
		t.Fatalf("panel.json =\n%s\nwant\n%s", got, want)
	}
	if again := f.run(); again.exitCode != 0 || !strings.Contains(again.output, "already this build") {
		t.Fatalf("second run: exit %d; output:\n%s", again.exitCode, again.output)
	}

	calls, _ := os.ReadFile(filepath.Join(f.binDir, "gh-calls.txt"))
	for _, line := range strings.Split(strings.TrimSpace(strings.ReplaceAll(string(calls), "\r\n", "\n")), "\n") {
		if !strings.HasPrefix(line, "api repos/example/panel/releases/latest") && !strings.HasPrefix(line, "release download v9.9.9-devyre.1 --repo example/panel --pattern management.html --output ") {
			t.Fatalf("unexpected gh call %q", line)
		}
	}
}
