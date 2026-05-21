package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"whatsapp-bridge/client"
)

type qrPayloadRequest struct {
	SessionID      string `json:"sessionId"      binding:"required"`
	TimeoutSeconds int    `json:"timeoutSeconds"` // optional; clamped to [5, 60]
}

// QRPayloadHandler returns a Gin handler for POST /api/qr-payload.
//
// It ensures a QR session is running for the given sessionId and blocks until
// the first QR code payload is available (up to timeoutSeconds, default 15 s).
// The returned payload is the raw QR string that the caller must convert into a
// QR image and present to the end-user for scanning.
func QRPayloadHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req qrPayloadRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "sessionId is required"})
			return
		}

		// Clamp the caller-supplied timeout to a safe range.
		timeout := 15 * time.Second
		if req.TimeoutSeconds >= 5 && req.TimeoutSeconds <= 60 {
			timeout = time.Duration(req.TimeoutSeconds) * time.Second
		}

		slog.Info("QRPayloadHandler: request received",
			"session", req.SessionID,
			"timeout", timeout,
		)

		payload, err := mgr.GetQRPayload(c.Request.Context(), req.SessionID, timeout)
		if err != nil {
			var pairingErr *client.PairingStateError
			if errors.As(err, &pairingErr) {
				// Session is already paired — instruct caller to reconnect.
				slog.Info("[QR_HANDLER] Session already paired, returning 409 Conflict",
					"session", req.SessionID,
					"phone", pairingErr.Phone,
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
			slog.Error("[QR_HANDLER_ERROR] GetQRPayload failed, returning 500 Internal Server Error",
				"session", req.SessionID,
				"requested_timeout", timeout.String(),
				"error", err.Error(),
				"error_type", fmt.Sprintf("%T", err),
			)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		slog.Info("[QR_HANDLER_SUCCESS] Returning QR payload, 200 OK",
			"session", req.SessionID,
			"payload_len", len(payload),
			"payload_prefix", func(p string) string {
				if len(p) > 20 {
					return p[:20]
				}
				return p
			}(payload),
		)
		c.JSON(http.StatusOK, gin.H{
			"qr_payload": payload,
			"sessionId":  req.SessionID,
		})
	}
}

// QRCurrentHandler returns a Gin handler for GET /api/qr-current/:session_id.
//
// Returns the latest QR payload for an active QR session without blocking.
// Useful for polling to obtain refreshed QR codes (WhatsApp rotates them ~20s).
// Returns 404 when no active QR payload exists for the session.
func QRCurrentHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("session_id")
		if sessionID == "" {
			slog.Error("[QR_CURRENT_ERROR] Missing session_id parameter")
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
			return
		}

		slog.Debug("[QR_CURRENT_DEBUG] Polling for current QR payload", "session", sessionID)
		payload, ok := mgr.GetCurrentQRPayload(sessionID)
		if !ok {
			slog.Debug("[QR_CURRENT_DEBUG] No active QR payload found, returning 404", "session", sessionID)
			c.JSON(http.StatusNotFound, gin.H{
				"error":     "no active QR payload for this session",
				"sessionId": sessionID,
			})
			return
		}

		slog.Debug("[QR_CURRENT_SUCCESS] Returning active QR payload",
			"session", sessionID,
			"payload_len", len(payload))
		c.JSON(http.StatusOK, gin.H{
			"qr_payload": payload,
			"sessionId":  sessionID,
		})
	}
}
