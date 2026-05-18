package handlers

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"whatsapp-bridge/client"
	"whatsapp-bridge/utils"
)

// sendMessageRequest covers text, audio, and image variants.
type sendMessageRequest struct {
	Phone   string      `json:"phone"`
	Message string      `json:"message"`     // text variant
	Type    string      `json:"type"`        // "audio" | "image" | empty = text
	Audio   *audioField `json:"audio"`       // audio variant
	Image   *imageField `json:"image"`       // image variant
	PTT     bool        `json:"ptt"`         // true → voice-note bubble (audio only)
}

type audioField struct {
	Data     string `json:"data"`     // base64-encoded OGG Opus
	MimeType string `json:"mimetype"` // always "audio/ogg; codecs=opus"
}

type imageField struct {
	Data     string `json:"data"`     // base64-encoded image bytes (PNG or JPEG)
	MimeType string `json:"mimetype"` // e.g. "image/png" or "image/jpeg"
	Caption  string `json:"caption"`  // optional caption text
}

// SendMessageHandler handles POST /send/message for BOTH text and audio.
// Dispatches based on the "type" field: absent/empty → text, "audio" → audio.
func SendMessageHandler(mgr *client.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.GetHeader("X-Device-Id")
		if deviceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "X-Device-Id header is required"})
			return
		}

		var req sendMessageRequest
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

		// Session must already exist in the pool (loaded at startup from its DB).
		// We do NOT lazily create sessions on the send path — use POST /api/pair-code first.
		if !mgr.SessionExists(deviceID) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session not configured — pair the device first"})
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

		if req.Type == "audio" {
			handleAudio(c, wac, jid.String(), req)
			return
		}

		if req.Type == "image" {
			handleImage(c, wac, jid.String(), req)
			return
		}

		if req.Message == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "message is required for text type"})
			return
		}

		handleText(c, wac, jid.String(), req.Message)
	}
}

func handleText(c *gin.Context, wac *whatsmeow.Client, jidStr, text string) {
	jid, _ := client.ParsePhone(jidStr)
	msg := client.BuildTextMessage(text)
	resp, err := wac.SendMessage(context.Background(), jid, msg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}

func handleAudio(c *gin.Context, wac *whatsmeow.Client, jidStr string, req sendMessageRequest) {
	if req.Audio == nil || req.Audio.Data == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "audio.data is required"})
		return
	}

	audioBytes, err := base64.StdEncoding.DecodeString(req.Audio.Data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "audio.data is not valid base64"})
		return
	}

	// Re-encode to OGG/Opus 48000 Hz mono 32 kbps — required for mobile playback.
	audioBytes, err = utils.TranscodeToWhatsAppOpus(audioBytes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "audio transcode failed: " + err.Error()})
		return
	}

	// Upload to WhatsApp CDN.
	uploaded, err := wac.Upload(context.Background(), audioBytes, whatsmeow.MediaAudio)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upload failed: " + err.Error()})
		return
	}

	duration := utils.ExtractOGGDuration(audioBytes)

	mimeType := req.Audio.MimeType
	if mimeType == "" {
		mimeType = "audio/ogg; codecs=opus"
	}

	audioMsg := &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:               proto.String(uploaded.URL),
			DirectPath:        proto.String(uploaded.DirectPath),
			MediaKey:          uploaded.MediaKey,
			FileEncSHA256:     uploaded.FileEncSHA256,
			FileSHA256:        uploaded.FileSHA256,
			FileLength:        proto.Uint64(uint64(len(audioBytes))),
			Mimetype:          proto.String(mimeType),
			PTT:               proto.Bool(req.PTT),
			Seconds:           proto.Uint32(duration),
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
			Waveform:          utils.GenerateWaveform(audioBytes),
		},
	}

	jid, _ := client.ParsePhone(jidStr)
	resp, err := wac.SendMessage(context.Background(), jid, audioMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}

// handleImage sends a PNG/JPEG image as a WhatsApp ImageMessage.
// The image bytes must be provided as a base64-encoded string in req.Image.Data.
func handleImage(c *gin.Context, wac *whatsmeow.Client, jidStr string, req sendMessageRequest) {
	if req.Image == nil || req.Image.Data == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image.data is required for type=image"})
		return
	}

	imgBytes, err := base64.StdEncoding.DecodeString(req.Image.Data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image.data is not valid base64"})
		return
	}

	// Upload to the WhatsApp CDN.
	uploaded, err := wac.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "image upload failed: " + err.Error()})
		return
	}

	mimeType := req.Image.MimeType
	if mimeType == "" {
		mimeType = "image/png"
	}

	caption := req.Image.Caption

	imageMsg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:               proto.String(uploaded.URL),
			DirectPath:        proto.String(uploaded.DirectPath),
			MediaKey:          uploaded.MediaKey,
			FileEncSHA256:     uploaded.FileEncSHA256,
			FileSHA256:        uploaded.FileSHA256,
			FileLength:        proto.Uint64(uint64(len(imgBytes))),
			Mimetype:          proto.String(mimeType),
			Caption:           proto.String(caption),
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		},
	}

	jid, _ := client.ParsePhone(jidStr)
	resp, err := wac.SendMessage(context.Background(), jid, imageMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}

