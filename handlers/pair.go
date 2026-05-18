package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"whatsapp-bridge/client"
)

type pairCodeRequest struct {
	SessionID   string `json:"sessionId"   binding:"required"`
	PhoneNumber string `json:"phoneNumber" binding:"required"`
}

// PairCodeHandler returns a Gin handler for POST /api/pair-code
func PairCodeHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req pairCodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			slog.Warn("PairCodeHandler: failed to parse request",
				"error", err,
			)
			c.JSON(http.StatusBadRequest, gin.H{"error": "sessionId and phoneNumber are required"})
			return
		}

		slog.Info("PairCodeHandler: request received",
			"session", req.SessionID,
			"phone_masked", req.PhoneNumber[:len(req.PhoneNumber)-4]+"****",
		)

		code, err := mgr.GeneratePairCode(c.Request.Context(), req.SessionID, req.PhoneNumber)
		if err != nil {
			var pairingErr *client.PairingStateError
			if errors.As(err, &pairingErr) {
				slog.Info("PairCodeHandler: returning 409 Conflict (already paired)",
					"session", req.SessionID,
					"error", err,
					"status", pairingErr.Status,
				)
				c.JSON(http.StatusConflict, gin.H{
					"error":            err.Error(),
					"sessionId":        req.SessionID,
					"status":           pairingErr.Status,
					"phone":            pairingErr.Phone,
					"action":           "reconnect",
					"pairing_required": false,
				})
				return
			}
			slog.Error("PairCodeHandler: returning 500 Internal Server Error",
				"session", req.SessionID,
				"error", err,
			)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		slog.Info("PairCodeHandler: successfully generated pairing code",
			"session", req.SessionID,
			"code_len", len(code),
		)
		c.JSON(http.StatusOK, gin.H{
			"code":      code,
			"sessionId": req.SessionID,
		})
	}
}
