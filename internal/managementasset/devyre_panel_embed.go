//go:build !devyre_nopanel

package managementasset

import _ "embed"

// devyrePanelIncluded reports that this build carries the panel (devyre_panel.go).
const devyrePanelIncluded = true

var (
	//go:embed devyre_panel/management.html
	devyrePanelHTML []byte

	//go:embed devyre_panel/panel.json
	devyrePanelRecord []byte
)
