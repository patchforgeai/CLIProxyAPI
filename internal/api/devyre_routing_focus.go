package api

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// devyreRegisterRoutingFocus registers /routing/focus on the v8 management
// group, behind its management auth. It has no /v0 counterpart.
func (s *Server) devyreRegisterRoutingFocus(v8 *gin.RouterGroup) {
	v8.GET("/routing/focus", s.mgmt.GetRoutingFocus)
	v8.PUT("/routing/focus", s.mgmt.PutRoutingFocus)
	v8.DELETE("/routing/focus", s.mgmt.DeleteRoutingFocus)
}

// devyreLoadRoutingFocus loads the focused credentials once at start-up into
// routingfocus.Default(), which the selectors and the management API share,
// from routingfocus.FileName in the configured auth dir. A problem never stops
// the server: an unreadable or invalid file is logged and the focus starts
// empty.
//
// Only a configured auth dir is used. The server binary always sets it (main
// resolves auth-dir, its default included, before the server is built), so an
// empty one means a config built in code, a test's or an embedder's: the
// focus then lives in memory only, and never reads or rewrites the default
// directory, which may hold another deployment's focus.
func devyreLoadRoutingFocus(cfg *config.Config) {
	authDir := ""
	if cfg != nil {
		authDir = strings.TrimSpace(cfg.AuthDir)
	}
	if authDir == "" {
		_ = routingfocus.Default().Load("")
		return
	}
	dir, err := util.ResolveAuthDir(authDir)
	if err != nil || dir == "" {
		log.WithError(err).Warn("routing focus: the auth directory does not resolve; the focus is kept in memory only")
		_ = routingfocus.Default().Load("")
		return
	}
	path := filepath.Join(dir, routingfocus.FileName)
	if errLoad := routingfocus.Default().Load(path); errLoad != nil {
		log.WithError(errLoad).Warn("routing focus: ignoring the saved focus; nothing is focused")
		return
	}
	for _, focus := range routingfocus.Default().List(time.Now()) {
		until := "none"
		if !focus.Until.IsZero() {
			until = focus.Until.UTC().Format(time.RFC3339)
		}
		log.Infof("routing focus: loaded | auth=%s provider=%s until=%s", focus.AuthID, focus.Provider, until)
	}
}
