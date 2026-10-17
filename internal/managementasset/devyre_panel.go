package managementasset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// The Devyre fork builds the management panel into the binary and never downloads or updates
// it. devyre_panel/management.html is the panel fork's release asset, embedded byte for byte by
// devyre_panel_embed.go; devyre_panel/panel.json records its version, source and sha256.
// devyre/scripts/update-panel.ps1 refreshes both. A build tagged devyre_nopanel leaves them out
// (devyre_panel_nopanel.go) for embedders that disable the control panel.
//
// Upstream's download and auto-update code stays in updater.go, so upstream syncs merge cleanly,
// but the fork never runs it: StartAutoUpdater and EnsureLatestManagementHTML return at once, and
// the server answers /management.html from here (internal/api/devyre_panel.go).

// panelOverrideEnv is upstream's variable that relocates the panel file.
const panelOverrideEnv = "MANAGEMENT_STATIC_PATH"

// EmbeddedPanel is the management panel built into this binary.
type EmbeddedPanel struct {
	// HTML is the panel, a single self-contained page.
	HTML []byte
	// Version is the panel fork's version from panel.json, such as v1.25.3-devyre.2.
	Version string
	// SHA256 is the lowercase hex sha256 of HTML.
	SHA256 string
}

type panelRecord struct {
	Version string `json:"version"`
	Source  string `json:"source"`
	SHA256  string `json:"sha256"`
}

var embeddedPanel = sync.OnceValue(func() *EmbeddedPanel {
	if !devyrePanelIncluded || len(devyrePanelHTML) == 0 {
		return nil
	}
	sum := sha256.Sum256(devyrePanelHTML)
	panel := &EmbeddedPanel{HTML: devyrePanelHTML, Version: "unknown", SHA256: hex.EncodeToString(sum[:])}
	var record panelRecord
	if errDecode := json.Unmarshal(devyrePanelRecord, &record); errDecode == nil && strings.TrimSpace(record.Version) != "" {
		panel.Version = strings.TrimSpace(record.Version)
	}
	return panel
})

// Panel returns the panel built into this binary, or nil in a build tagged devyre_nopanel.
// The returned value is shared: never modify its HTML.
func Panel() *EmbeddedPanel {
	return embeddedPanel()
}

// PanelOverridePath returns the panel file that MANAGEMENT_STATIC_PATH names, and whether the
// variable is set. That explicit override is served instead of the embedded panel while the file
// exists; it is never downloaded or updated.
func PanelOverridePath() (string, bool) {
	if strings.TrimSpace(os.Getenv(panelOverrideEnv)) == "" {
		return "", false
	}
	return FilePath(""), true
}

// devyreSkipAutoUpdater stands in for StartAutoUpdater: no updater starts, because the fork
// never downloads or updates the panel. It logs which panel this server serves and always
// reports true.
func devyreSkipAutoUpdater() bool {
	if cfg := currentConfigPtr.Load(); cfg != nil && (cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel) {
		log.Debug("management panel: disabled by config")
		return true
	}
	built := "none (built with devyre_nopanel), so /management.html answers 404"
	if panel := Panel(); panel != nil {
		built = fmt.Sprintf("the embedded %s (sha256 %s)", panel.Version, panel.SHA256)
	}
	if path, ok := PanelOverridePath(); ok {
		log.Infof("management panel: %s (%s) while that file exists, otherwise %s; never downloaded", path, panelOverrideEnv, built)
	} else {
		log.Infof("management panel: %s; never downloaded", built)
	}
	return true
}

// devyreRefuseDownload keeps EnsureLatestManagementHTML, the only path to GitHub in this
// package, from downloading anything, whoever calls it. It always reports true.
func devyreRefuseDownload() bool {
	log.Debug("management asset download skipped: the panel is built into the binary")
	return true
}
