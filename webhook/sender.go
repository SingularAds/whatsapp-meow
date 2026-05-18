package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// MessagePayload mirrors the payload object that FastAPI expects.
type MessagePayload struct {
	ChatID      string `json:"chat_id"`
	From        string `json:"from"`      // sender's full JID e.g. "916387400721@s.whatsapp.net" or "134544296509456@lid"
	SenderPN    string `json:"sender_pn"` // resolved phone-number JID when sender used @lid privacy mode (e.g. "917696794756@s.whatsapp.net"); empty when not needed or not yet cached
	PushName    string `json:"push_name"`
	Body        string `json:"body"`
	MessageID   string `json:"message_id"`
	Timestamp   int64  `json:"timestamp"` // Unix epoch seconds
	IsFromMe    bool   `json:"is_from_me"`
	IsGroup     bool   `json:"is_group"`
	MessageType string `json:"message_type"`
	MediaURL    string `json:"media_url"`
	MimeType    string `json:"mime_type"`
}

// Event is the top-level envelope sent to FastAPI.
type Event struct {
	Event    string         `json:"event"`
	DeviceID string         `json:"device_id"`
	Phone    string         `json:"phone"` // bridge's own WhatsApp number (business number that received the message)
	Payload  MessagePayload `json:"payload"`
}

// CallPayload is the payload for call-related webhook events (call_offer, call_missed).
type CallPayload struct {
	CallID      string `json:"call_id"`
	CallerJID   string `json:"caller_jid"`   // full non-AD JID of the caller (e.g. "917696794756@s.whatsapp.net")
	CallerPhone string `json:"caller_phone"` // raw digits only (same as chat_id for messages)
	PushName    string `json:"push_name"`    // caller's display name (may be empty for call events)
	Timestamp   int64  `json:"timestamp"`    // Unix epoch seconds
}

// CallEvent is the top-level envelope for call-related events sent to FastAPI.
type CallEvent struct {
	Event    string      `json:"event"`
	DeviceID string      `json:"device_id"`
	Phone    string      `json:"phone"` // bridge's own WhatsApp number
	Payload  CallPayload `json:"payload"`
}

// Sender posts webhook events to the FastAPI backend.
type Sender struct {
	webhookURL    string
	webhookSecret string
	client        *http.Client
}

// NewSender creates a Sender. webhookSecret may be empty.
func NewSender(webhookURL, webhookSecret string) *Sender {
	return &Sender{
		webhookURL:    webhookURL,
		webhookSecret: webhookSecret,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

// Send posts the event to <webhookURL>/whatsmeow-webhook.
// Retries up to 3 times (1 s delay between attempts) on network errors or non-200 responses.
// A final failure is logged but does NOT return an error so the caller is never blocked.
func (s *Sender) Send(evt Event) {
	body, err := json.Marshal(evt)
	if err != nil {
		slog.Error("webhook marshal failed", "error", err)
		return
	}

	url := s.webhookURL + "/whatsmeow-webhook"
	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Second)
		}

		// Structured log for every webhook POST attempt for easier debugging.
		slog.Info("posting webhook",
			"attempt", attempt,
			"url", url,
			"device_id", evt.DeviceID,
			"event", evt.Event,
			"chat_id", evt.Payload.ChatID,
			"message_id", evt.Payload.MessageID,
		)

		if err := s.post(url, body); err != nil {
			slog.Warn("webhook attempt failed",
				"attempt", attempt,
				"url", url,
				"error", err,
			)
			if attempt == maxAttempts {
				slog.Error("webhook permanently failed after retries",
					"device_id", evt.DeviceID,
					"message_id", evt.Payload.MessageID,
				)
			}
			continue
		}
		return // success
	}
}

func (s *Sender) post(url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.webhookSecret != "" {
		req.Header.Set("X-Webhook-Secret", s.webhookSecret)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// SendCall posts a call-related event (call_offer, call_missed) to the FastAPI backend.
// Retries up to 3 times (1 s delay) on failure.
func (s *Sender) SendCall(evt CallEvent) {
	body, err := json.Marshal(evt)
	if err != nil {
		slog.Error("call webhook marshal failed", "error", err)
		return
	}

	url := s.webhookURL + "/whatsmeow-webhook"
	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Second)
		}

		slog.Info("posting call webhook",
			"attempt", attempt,
			"url", url,
			"device_id", evt.DeviceID,
			"event", evt.Event,
			"call_id", evt.Payload.CallID,
			"caller_phone", evt.Payload.CallerPhone,
		)

		if err := s.post(url, body); err != nil {
			slog.Warn("call webhook attempt failed",
				"attempt", attempt,
				"url", url,
				"error", err,
			)
			if attempt == maxAttempts {
				slog.Error("call webhook permanently failed after retries",
					"device_id", evt.DeviceID,
					"call_id", evt.Payload.CallID,
				)
			}
			continue
		}
		return // success
	}
}
