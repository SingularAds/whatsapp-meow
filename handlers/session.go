package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"whatsapp-bridge/client"
)

// SessionHandler returns a Gin handler for GET /api/sessions/:session_id
func SessionHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("session_id")
		if sessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
			return
		}

		status, phone, paired := mgr.GetStatus(sessionID)
		c.JSON(http.StatusOK, gin.H{
			"sessionId":        sessionID,
			"status":           status,
			"phone":            phone,
			"paired":           paired,
			"pairing_required": !paired,
		})
	}
}

// SessionsListHandler returns a Gin handler for GET /api/sessions (list all).
func SessionsListHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"sessions": mgr.ListSessions()})
	}
}

// SessionReconnectHandler returns a Gin handler for POST /api/sessions/:session_id/reconnect.
func SessionReconnectHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("session_id")
		if sessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
			return
		}

		if err := mgr.ReconnectSession(c.Request.Context(), sessionID); err != nil {
			c.JSON(http.StatusConflict, gin.H{
				"error":     err.Error(),
				"sessionId": sessionID,
			})
			return
		}

		status, phone, paired := mgr.GetStatus(sessionID)
		c.JSON(http.StatusAccepted, gin.H{
			"sessionId":        sessionID,
			"status":           status,
			"phone":            phone,
			"paired":           paired,
			"pairing_required": !paired,
			"action":           "reconnect",
		})
	}
}

// SessionLogoutHandler returns a Gin handler for POST /api/sessions/:session_id/logout.
// It fully unpairs the session so that a fresh pair-code flow can be run.
func SessionLogoutHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("session_id")
		if sessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
			return
		}

		if err := mgr.LogoutSession(c.Request.Context(), sessionID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":     err.Error(),
				"sessionId": sessionID,
			})
			return
		}

		status, phone, paired := mgr.GetStatus(sessionID)
		c.JSON(http.StatusOK, gin.H{
			"sessionId":        sessionID,
			"status":           status,
			"phone":            phone,
			"paired":           paired,
			"pairing_required": !paired,
			"action":           "logout",
		})
	}
}
