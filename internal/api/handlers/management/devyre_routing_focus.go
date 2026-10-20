package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// devyre: /v8/management/routing/focus focuses one credential per provider:
// the expiring-first selector then uses it for every request it can serve
// (internal/routingfocus, sdk/cliproxy/auth/devyre_focus.go).

// routingFocusResponse is the body of every /v8/management/routing/focus answer.
type routingFocusResponse struct {
	Focus []routingFocusEntry `json:"focus"`
}

// routingFocusEntry is one active focus whose credential still exists.
type routingFocusEntry struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	// Name is the credential's file name as the credential list names it, or
	// its ID when it has no file.
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Until is null for a focus without an end.
	Until *time.Time `json:"until"`
	SetAt time.Time  `json:"set_at"`
}

// routingFocusRequest is the body of PUT /v8/management/routing/focus.
type routingFocusRequest struct {
	// Name is the credential's ID or file name.
	Name string `json:"name"`
	// Until is an RFC 3339 time in the future, or null (or absent) for a focus
	// without an end.
	Until *string `json:"until"`
}

// SetRoutingFocusStore makes the handler read and change store instead of
// routingfocus.Default(), the store the selectors read; nil restores it. It is
// for tests and embedders that give their selectors their own store.
func (h *Handler) SetRoutingFocusStore(store *routingfocus.Store) {
	if h == nil {
		return
	}
	h.devyreState().focus.Store(store)
}

func (h *Handler) devyreFocusStore() *routingfocus.Store {
	if store := h.devyreState().focus.Load(); store != nil {
		return store
	}
	return routingfocus.Default()
}

func (h *Handler) devyreAuthManager() *coreauth.Manager {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authManager
}

// GetRoutingFocus handles GET /v8/management/routing/focus.
func (h *Handler) GetRoutingFocus(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	c.JSON(http.StatusOK, h.routingFocusBody(time.Now()))
}

// PutRoutingFocus handles PUT /v8/management/routing/focus: it focuses the
// credential the body names, replacing any focus of its provider.
func (h *Handler) PutRoutingFocus(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	manager := h.devyreAuthManager()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req routingFocusRequest
	data, errRead := c.GetRawData()
	if errRead != nil || len(bytes.TrimSpace(data)) == 0 || json.Unmarshal(data, &req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	now := time.Now()
	var until time.Time
	if req.Until != nil {
		parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(*req.Until))
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be an RFC 3339 time or null"})
			return
		}
		if !parsed.After(now) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be in the future"})
			return
		}
		until = parsed
	}

	target := routingFocusLookup(manager, name)
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(target) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}
	provider := routingfocus.NormalizeProvider(target.Provider)
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential has no provider"})
		return
	}
	if errSet := h.devyreFocusStore().Set(routingfocus.Focus{AuthID: target.ID, Provider: provider, Until: until, SetAt: now}); errSet != nil {
		log.WithError(errSet).Warn("routing focus: could not save the focus")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the focus"})
		return
	}
	untilField := "none"
	if !until.IsZero() {
		untilField = until.UTC().Format(time.RFC3339)
	}
	log.WithFields(log.Fields{"auth": target.ID, "provider": provider, "until": untilField}).
		Infof("routing focus: set | auth=%s provider=%s until=%s", target.ID, provider, untilField)
	c.JSON(http.StatusOK, h.routingFocusBody(time.Now()))
}

// DeleteRoutingFocus handles DELETE /v8/management/routing/focus?name=<auth ID
// or file name> or ?provider=<provider>, exactly one of them. It succeeds
// whether or not there was a focus to remove.
func (h *Handler) DeleteRoutingFocus(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	name := strings.TrimSpace(c.Query("name"))
	provider := routingfocus.NormalizeProvider(c.Query("provider"))
	if (name == "") == (provider == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one of name and provider is required"})
		return
	}
	store := h.devyreFocusStore()
	var errClear error
	if name != "" {
		authID := name
		if target := routingFocusLookup(h.devyreAuthManager(), name); target != nil {
			authID = target.ID
		}
		errClear = store.ClearAuth(authID)
		if errClear == nil {
			log.WithField("auth", authID).Infof("routing focus: cleared | auth=%s", authID)
		}
	} else {
		errClear = store.ClearProvider(provider)
		if errClear == nil {
			log.WithField("provider", provider).Infof("routing focus: cleared | provider=%s", provider)
		}
	}
	if errClear != nil {
		log.WithError(errClear).Warn("routing focus: could not save the cleared focus")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the focus"})
		return
	}
	c.JSON(http.StatusOK, h.routingFocusBody(time.Now()))
}

// routingFocusLookup finds a credential the way PatchAuthFileFields does: by
// ID, then by file name.
func routingFocusLookup(manager *coreauth.Manager, name string) *coreauth.Auth {
	if manager == nil || name == "" {
		return nil
	}
	if auth, ok := manager.GetByID(name); ok && auth != nil {
		return auth
	}
	for _, auth := range manager.List() {
		if auth != nil && auth.FileName == name {
			return auth
		}
	}
	return nil
}

// routingFocusBody lists the focuses active at now whose credential still
// exists, by provider.
func (h *Handler) routingFocusBody(now time.Time) routingFocusResponse {
	response := routingFocusResponse{Focus: []routingFocusEntry{}}
	manager := h.devyreAuthManager()
	if manager == nil {
		return response
	}
	for _, focus := range h.devyreFocusStore().List(now) {
		auth, ok := manager.GetByID(focus.AuthID)
		if !ok || auth == nil {
			continue
		}
		response.Focus = append(response.Focus, routingFocusEntry{
			AuthID:    focus.AuthID,
			AuthIndex: lockedAuthIndex(auth),
			Name:      authFileListName(auth),
			Provider:  focus.Provider,
			Until:     optionalUTCTime(focus.Until),
			SetAt:     focus.SetAt.UTC(),
		})
	}
	return response
}

// forgetRemovedFocus removes the focus of every credential that auths, listed
// at listedAt, no longer holds. An empty list removes nothing: it is what a
// failed credential load looks like, and without credentials no focus applies
// anyway.
func forgetRemovedFocus(store *routingfocus.Store, auths []*coreauth.Auth, listedAt time.Time) {
	if store.Empty() || len(auths) == 0 {
		return
	}
	existing := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			existing = append(existing, auth.ID)
		}
	}
	if err := store.Retain(existing, listedAt); err != nil {
		log.WithError(err).Warn("routing focus: could not forget removed credentials")
	}
}
