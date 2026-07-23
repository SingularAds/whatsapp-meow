package handlers

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"whatsapp-bridge/client"
	"whatsapp-bridge/utils"
)

// retryDelays defines the wait between SendMessage attempts when WhatsApp
// returns the recoverable error 463 (NackCallerReachoutTimelocked).
//
// The first send to a cold contact has no stored privacy token, so whatsmeow's
// send.go schedules an asynchronous issuePrivacyTokenAndSave call AFTER the
// failed send. The IQ round-trip + server-side privacy_token notification
// typically settles in 1–5 seconds, so a single short retry usually captures
// any wins this mitigation can capture. Beyond that, additional retries do
// not help — the server has decided.
//
// IMPORTANT: the total budget (sum of these delays + per-attempt send time)
// MUST stay under the backend's httpx client timeout (currently 30s in
// app/services/whatsmeow_client.py `_client(timeout=30.0)`). Otherwise the
// backend cancels the request mid-retry, the bridge sees "context canceled",
// the 429 mapping never fires, and the backend's _send treats it as a
// generic crash instead of a typed reachout-timelocked failure. 7s × 1 retry
// + ~2s of send time = ~9s — well inside the 30s budget.
var retryDelays = []time.Duration{7 * time.Second}

// isReachoutTimelocked reports whether the error returned by whatsmeow's
// SendMessage corresponds to WhatsApp server error 463 (cold-contact
// rate-limit on companion devices). The library surfaces this as the literal
// string "server returned error 463" inside the wrapped error.
func isReachoutTimelocked(err error) bool {
	return err != nil && strings.Contains(err.Error(), "server returned error 463")
}

// sendWithRetry calls wac.SendMessage and, on a 463, waits and retries up to
// len(retryDelays) more times. Returns the SendResponse and the final error
// (nil on success). The opTimeout caps each individual SendMessage attempt;
// the wait between attempts is independent and uses a separate timer.
//
// Aborts retries early if the parent request context is cancelled — this
// prevents the bridge from holding a closed HTTP connection open.
func sendWithRetry(parentCtx context.Context, wac *whatsmeow.Client, jid types.JID, msg *waE2E.Message, opTimeout time.Duration, payloadType string) (whatsmeow.SendResponse, error) {
	var resp whatsmeow.SendResponse
	var lastErr error

	maxAttempts := 1 + len(retryDelays)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sendCtx, cancel := context.WithTimeout(parentCtx, opTimeout)
		var err error
		resp, err = wac.SendMessage(sendCtx, jid, msg)
		cancel()
		if err == nil {
			if attempt > 1 {
				slog.Info("[bridge] SendMessage succeeded after retry",
					"to", jid.String(),
					"payload_type", payloadType,
					"attempt", attempt,
				)
			}
			return resp, nil
		}
		lastErr = err

		if !isReachoutTimelocked(err) {
			return resp, err
		}

		if attempt >= maxAttempts {
			break
		}

		delay := retryDelays[attempt-1]
		slog.Warn("[bridge] SendMessage 463 — scheduling retry",
			"to", jid.String(),
			"payload_type", payloadType,
			"attempt", attempt,
			"next_attempt_in", delay.String(),
		)
		select {
		case <-time.After(delay):
		case <-parentCtx.Done():
			return resp, parentCtx.Err()
		}
	}
	return resp, lastErr
}

// writeSendError writes a structured error response. For 463 (cold-contact
// rate-limit) we return HTTP 429 so the backend can distinguish this from a
// generic bridge crash. The Python backend should:
//   - Not advance any state machine on 429 (the message did not get delivered)
//   - Avoid an immediate user-visible retry — there's no fix that completes in
//     seconds for this specific failure mode
//   - Optionally schedule a delayed background retry (privacy tokens may be
//     issued async after our retries, so a send 5–15 min later sometimes works)
func writeSendError(c *gin.Context, payloadType string, jid types.JID, textLen int, err error) {
	slog.Error("[bridge] SendMessage failed",
		"to", jid.String(),
		"payload_type", payloadType,
		"text_len", textLen,
		"error", err.Error(),
	)
	if isReachoutTimelocked(err) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":               "reachout_timelocked",
			"code":                463,
			"message":             "WhatsApp rejected the send (companion-device privacy gating). Recipient may not accept messages from this device yet.",
			"retry_after_seconds": 300,
		})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// sendMessageRequest covers text, audio, image, and video variants.
type sendMessageRequest struct {
	Phone   string      `json:"phone"`
	Message string      `json:"message"` // text variant
	Type    string      `json:"type"`    // "audio" | "image" | "video" | empty = text
	Audio   *audioField `json:"audio"`   // audio variant
	Image   *imageField `json:"image"`   // image variant
	Video   *videoField `json:"video"`   // video variant
	PTT     bool        `json:"ptt"`     // true → voice-note bubble (audio only)
	PTV     bool        `json:"ptv"`     // true → round video-note bubble (video only)
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

type videoField struct {
	Data     string `json:"data"`     // base64-encoded video bytes (MP4/H.264)
	MimeType string `json:"mimetype"` // e.g. "video/mp4"
	Caption  string `json:"caption"`  // optional caption (ignored for round PTV notes)
	Seconds  uint32 `json:"seconds"`  // optional duration hint
	Width    uint32 `json:"width"`    // optional pixel width
	Height   uint32 `json:"height"`   // optional pixel height
}

// SendMessageHandler handles POST /send/message for BOTH text and audio.
// Dispatches based on the "type" field: absent/empty → text, "audio" → audio.
func SendMessageHandler(mgr *client.Manager, opTimeout time.Duration) gin.HandlerFunc {
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
			handleAudio(c, wac, jid.String(), req, opTimeout)
			return
		}

		if req.Type == "image" {
			handleImage(c, wac, jid.String(), req, opTimeout)
			return
		}

		if req.Type == "video" {
			handleVideo(c, wac, jid.String(), req, opTimeout)
			return
		}

		if req.Message == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "message is required for text type"})
			return
		}

		handleText(c, wac, jid.String(), req.Message, opTimeout)
	}
}

func handleText(c *gin.Context, wac *whatsmeow.Client, jidStr, text string, opTimeout time.Duration) {
	jid, _ := client.ParsePhone(jidStr)
	msg := client.BuildTextMessage(text)
	payloadType := "conversation"
	if msg.GetExtendedTextMessage() != nil {
		payloadType = "extended_text"
	}
	resp, err := sendWithRetry(c.Request.Context(), wac, jid, msg, opTimeout, payloadType)
	if err != nil {
		writeSendError(c, payloadType, jid, len(text), err)
		return
	}
	slog.Info("[bridge] SendMessage ok",
		"to", jid.String(),
		"payload_type", payloadType,
		"message_id", resp.ID,
	)
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}

func handleAudio(c *gin.Context, wac *whatsmeow.Client, jidStr string, req sendMessageRequest, opTimeout time.Duration) {
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
	uploadCtx, uploadCancel := context.WithTimeout(c.Request.Context(), opTimeout)
	defer uploadCancel()
	uploaded, err := wac.Upload(uploadCtx, audioBytes, whatsmeow.MediaAudio)
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
	resp, err := sendWithRetry(c.Request.Context(), wac, jid, audioMsg, opTimeout, "audio")
	if err != nil {
		writeSendError(c, "audio", jid, 0, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}

// handleImage sends a PNG/JPEG image as a WhatsApp ImageMessage.
// The image bytes must be provided as a base64-encoded string in req.Image.Data.
func handleImage(c *gin.Context, wac *whatsmeow.Client, jidStr string, req sendMessageRequest, opTimeout time.Duration) {
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
	uploadCtx, uploadCancel := context.WithTimeout(c.Request.Context(), opTimeout)
	defer uploadCancel()
	uploaded, err := wac.Upload(uploadCtx, imgBytes, whatsmeow.MediaImage)
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
	resp, err := sendWithRetry(c.Request.Context(), wac, jid, imageMsg, opTimeout, "image")
	if err != nil {
		writeSendError(c, "image", jid, len(caption), err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}

// handleVideo sends an MP4 video. When req.PTV is true it is delivered as a
// WhatsApp "video note" — the round, tap-to-play bubble (proto PtvMessage) —
// which WhatsApp only renders correctly for SHORT (≤60s), SQUARE clips. When
// false it is a normal VideoMessage (with optional caption). The bytes must be
// a base64-encoded, already-WhatsApp-compatible MP4 (H.264/AAC); the bridge does
// NOT transcode video, so the caller is responsible for format.
func handleVideo(c *gin.Context, wac *whatsmeow.Client, jidStr string, req sendMessageRequest, opTimeout time.Duration) {
	if req.Video == nil || req.Video.Data == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video.data is required for type=video"})
		return
	}

	vidBytes, err := base64.StdEncoding.DecodeString(req.Video.Data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video.data is not valid base64"})
		return
	}

	// Upload to the WhatsApp CDN.
	uploadCtx, uploadCancel := context.WithTimeout(c.Request.Context(), opTimeout)
	defer uploadCancel()
	uploaded, err := wac.Upload(uploadCtx, vidBytes, whatsmeow.MediaVideo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "video upload failed: " + err.Error()})
		return
	}

	mimeType := req.Video.MimeType
	if mimeType == "" {
		mimeType = "video/mp4"
	}

	vm := &waE2E.VideoMessage{
		URL:               proto.String(uploaded.URL),
		DirectPath:        proto.String(uploaded.DirectPath),
		MediaKey:          uploaded.MediaKey,
		FileEncSHA256:     uploaded.FileEncSHA256,
		FileSHA256:        uploaded.FileSHA256,
		FileLength:        proto.Uint64(uint64(len(vidBytes))),
		Mimetype:          proto.String(mimeType),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
	}
	if req.Video.Seconds > 0 {
		vm.Seconds = proto.Uint32(req.Video.Seconds)
	}
	if req.Video.Width > 0 {
		vm.Width = proto.Uint32(req.Video.Width)
	}
	if req.Video.Height > 0 {
		vm.Height = proto.Uint32(req.Video.Height)
	}

	var msg *waE2E.Message
	payloadType := "video"
	if req.PTV {
		// Round video note. PTV bubbles never show a caption, so it is dropped.
		payloadType = "ptv"
		msg = &waE2E.Message{PtvMessage: vm}
	} else {
		if req.Video.Caption != "" {
			vm.Caption = proto.String(req.Video.Caption)
		}
		msg = &waE2E.Message{VideoMessage: vm}
	}

	jid, _ := client.ParsePhone(jidStr)
	resp, err := sendWithRetry(c.Request.Context(), wac, jid, msg, opTimeout, payloadType)
	if err != nil {
		writeSendError(c, payloadType, jid, len(req.Video.Caption), err)
		return
	}
	slog.Info("[bridge] SendMessage ok",
		"to", jid.String(),
		"payload_type", payloadType,
		"message_id", resp.ID,
	)
	c.JSON(http.StatusOK, gin.H{"status": "sent", "message_id": resp.ID})
}
