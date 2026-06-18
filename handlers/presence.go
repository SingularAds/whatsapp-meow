package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mau.fi/whatsmeow/types"

	"whatsapp-bridge/client"
)

// presenceRequest is the body of POST /send/presence.
//
// state values:
//
//	"composing" (default) — shows "typing…" bubble
//	"recording"           — shows "recording audio…"
//	"paused"              — stops the indicator
//
// The recipient's WhatsApp client auto-expires the bubble after ~10 seconds
// if no further presence updates arrive, so callers normally only need to
// fire one "composing" before sending the actual message.
type presenceRequest struct {
	Phone string `json:"phone"`
	State string `json:"state"`
}

// SendPresenceHandler exposes wac.SendChatPresence over HTTP so the Python
// backend can show a typing indicator while the LLM is generating a reply.
// Fire-and-forget on the Python side: any failure here is non-fatal.
func SendPresenceHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.GetHeader("X-Device-Id")
		if deviceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "X-Device-Id header is required"})
			return
		}

		var req presenceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if req.Phone == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid phone number"})
			return
		}

		jid, err := client.ParsePhone(req.Phone)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid phone number"})
			return
		}

		if !mgr.SessionExists(deviceID) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session not configured"})
			return
		}

		wac, err := mgr.GetClient(deviceID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "WhatsApp not connected"})
			return
		}
		if !wac.IsConnected() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "WhatsApp not connected"})
			return
		}

		var (
			state types.ChatPresence
			media types.ChatPresenceMedia
		)
		switch req.State {
		case "", "composing", "typing":
			state = types.ChatPresenceComposing
			media = types.ChatPresenceMediaText
		case "recording":
			state = types.ChatPresenceComposing
			media = types.ChatPresenceMediaAudio
		case "paused", "stop":
			state = types.ChatPresencePaused
			media = types.ChatPresenceMediaText
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown state: " + req.State})
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		if err := wac.SendChatPresence(ctx, jid, state, media); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": req.State})
	}
}
