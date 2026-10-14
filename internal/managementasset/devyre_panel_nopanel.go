//go:build devyre_nopanel

package managementasset

// devyrePanelIncluded reports that this build leaves the panel out: `go build -tags
// devyre_nopanel` is for embedders that disable the control panel, such as a desktop app's
// sidecar. The binary carries no panel bytes and /management.html answers 404 (devyre_panel.go).
const devyrePanelIncluded = false

var (
	devyrePanelHTML   []byte
	devyrePanelRecord []byte
)
