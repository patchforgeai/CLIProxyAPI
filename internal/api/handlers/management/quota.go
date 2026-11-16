package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

// ResetQuota clears quota/cooldown routing state for one auth index.
func (h *Handler) ResetQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
		// devyre: when the quota started over (devyre_quota_reset.go).
		ObservedBefore string `json:"observed_before"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	// devyre: a reset learned late leaves standing a rejection made since (devyre_quota_reset.go).
	resetAt, kept, errAt := h.devyreResetAt(auth, req.ObservedBefore)
	if errAt != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errAt.Error()})
		return
	}
	if kept {
		c.JSON(http.StatusOK, gin.H{"status": "kept", "auth_index": authIndex, "models": []string{}})
		return
	}

	updated, models, errReset := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
	if errReset != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset quota: %v", errReset)})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	updated.EnsureIndex()
	// devyre: what was read of its quota before no longer describes it (devyre_quota_reset.go).
	h.devyreQuotaReset(updated, resetAt)

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"auth_index": updated.Index,
		"models":     models,
	})
}
