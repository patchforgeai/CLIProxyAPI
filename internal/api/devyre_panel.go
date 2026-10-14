package api

import (
	"bytes"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
	log "github.com/sirupsen/logrus"
)

// devyreServePanel answers /management.html for serveManagementControlPanel once the control
// panel is known to be enabled. It always answers and reports true, so upstream's
// file-or-download path after it never runs: this server never downloads the panel.
//
//   - MANAGEMENT_STATIC_PATH, when set and naming an existing file, is served as upstream
//     serves it: an explicit override for trying a panel build without rebuilding the server.
//   - Otherwise the panel built into the binary (internal/managementasset/devyre_panel.go),
//     with its sha256 as a strong ETag, so a browser revalidates and gets a 304.
//   - A build tagged devyre_nopanel has no panel: 404.
func devyreServePanel(c *gin.Context) bool {
	panel := managementasset.Panel()
	if path, ok := managementasset.PanelOverridePath(); ok {
		if info, errStat := os.Stat(path); errStat == nil && info.Mode().IsRegular() {
			c.File(path)
			return true
		}
		if panel != nil {
			log.Warnf("management panel: no file at %s (MANAGEMENT_STATIC_PATH); serving the built-in panel", path)
		} else {
			log.Warnf("management panel: no file at %s (MANAGEMENT_STATIC_PATH) and none built in; answering 404", path)
		}
	}
	if panel == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return true
	}
	header := c.Writer.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("ETag", `"`+panel.SHA256+`"`)
	http.ServeContent(c.Writer, c.Request, managementasset.ManagementFileName, time.Time{}, bytes.NewReader(panel.HTML))
	return true
}
