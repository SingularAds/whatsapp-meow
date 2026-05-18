package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"whatsapp-bridge/client"
)

type defaultPairCodeRequest struct {
	PhoneNumber string `json:"phoneNumber" binding:"required"`
}

// DefaultSessionStatusHandler returns status for the configured default session.
// It ensures the session is initialised before returning status.
func DefaultSessionStatusHandler(mgr *client.Manager, defaultSessionID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if defaultSessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "DEFAULT_SESSION_ID is not configured"})
			return
		}

		if _, err := mgr.EnsureSession(c.Request.Context(), defaultSessionID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":     err.Error(),
				"sessionId": defaultSessionID,
			})
			return
		}

		status, phone, paired := mgr.GetStatus(defaultSessionID)
		action := "none"
		if !paired {
			action = "pair"
		} else if status != client.StatusConnected {
			action = "reconnect"
		}

		c.JSON(http.StatusOK, gin.H{
			"sessionId":         defaultSessionID,
			"status":            status,
			"phone":             phone,
			"paired":            paired,
			"pairing_required":  !paired,
			"recommendedAction": action,
		})
	}
}

// DefaultSessionReconnectHandler reconnects the configured default session.
func DefaultSessionReconnectHandler(mgr *client.Manager, defaultSessionID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if defaultSessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "DEFAULT_SESSION_ID is not configured"})
			return
		}

		if err := mgr.ReconnectSession(c.Request.Context(), defaultSessionID); err != nil {
			c.JSON(http.StatusConflict, gin.H{
				"error":     err.Error(),
				"sessionId": defaultSessionID,
			})
			return
		}

		status, phone, paired := mgr.GetStatus(defaultSessionID)
		c.JSON(http.StatusAccepted, gin.H{
			"sessionId":        defaultSessionID,
			"status":           status,
			"phone":            phone,
			"paired":           paired,
			"pairing_required": !paired,
			"action":           "reconnect",
		})
	}
}

// DefaultSessionPairCodeHandler generates a pair code for the configured default session.
func DefaultSessionPairCodeHandler(mgr *client.Manager, defaultSessionID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if defaultSessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "DEFAULT_SESSION_ID is not configured"})
			return
		}

		var req defaultPairCodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "phoneNumber is required"})
			return
		}

		code, err := mgr.GeneratePairCode(c.Request.Context(), defaultSessionID, req.PhoneNumber)
		if err != nil {
			var pairingErr *client.PairingStateError
			if errors.As(err, &pairingErr) {
				c.JSON(http.StatusConflict, gin.H{
					"error":            err.Error(),
					"sessionId":        defaultSessionID,
					"status":           pairingErr.Status,
					"phone":            pairingErr.Phone,
					"action":           "reconnect",
					"pairing_required": false,
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"code":      code,
			"sessionId": defaultSessionID,
		})
	}
}
