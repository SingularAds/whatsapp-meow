// Package analytics provides a lightweight, non-blocking PostHog event tracker.
//
// Events are submitted fire-and-forget in a background goroutine so they never
// block the WhatsApp message processing pipeline. If PostHog is not configured
// (no API key) every method is a no-op.
package analytics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	defaultHost   = "https://app.posthog.com"
	capturePath   = "/capture/"
	clientTimeout = 5 * time.Second
)

// ─────────────────────────────────────────────────────────────────────────────
// Event name constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	EventMessageReceived     = "whatsmeow_message_received"
	EventMessageTypeFiltered = "whatsmeow_message_type_filtered"
	EventIntentClassified    = "whatsmeow_intent_classified"
	EventBusinessIntent      = "whatsmeow_business_intent_detected"
	EventPersonalIntent      = "whatsmeow_personal_intent_detected"
	EventUnclearIntent       = "whatsmeow_unclear_intent_detected"
	EventAIReplySent         = "whatsmeow_ai_reply_sent"
	EventAIReplySkipped      = "whatsmeow_ai_reply_skipped"
	EventWebhookError        = "whatsmeow_webhook_error"
	EventConversationCached  = "whatsmeow_conversation_state_cached"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tracker
// ─────────────────────────────────────────────────────────────────────────────

// Tracker wraps the PostHog capture API. All methods are safe to call
// concurrently. If the tracker was created without an API key every call is a
// no-op — the rest of the codebase never needs to guard against a nil Tracker.
type Tracker struct {
	apiKey  string
	host    string
	client  *http.Client
	enabled bool
}

// NewTracker creates a Tracker. If apiKey is empty the tracker is disabled and
// all Track calls are silently dropped. host may be empty — it defaults to the
// PostHog US cloud endpoint.
func NewTracker(apiKey, host string) *Tracker {
	if host == "" {
		host = defaultHost
	}
	return &Tracker{
		apiKey:  apiKey,
		host:    host,
		enabled: apiKey != "",
		client:  &http.Client{Timeout: clientTimeout},
	}
}

// Track fires a PostHog event asynchronously. Errors are logged at WARN level
// but never propagate to the caller — message processing must never be blocked
// by analytics failures.
func (t *Tracker) Track(event, distinctID string, properties map[string]interface{}) {
	if !t.enabled {
		return
	}
	// Run in background so the caller is never blocked.
	go t.send(event, distinctID, properties)
}

// ─────────────────────────────────────────────────────────────────────────────
// Convenience methods — each maps to a named event constant above.
// ─────────────────────────────────────────────────────────────────────────────

// TrackMessageReceived is called when a new inbound message arrives at the bridge.
func (t *Tracker) TrackMessageReceived(deviceID, chatID, msgType, pushName string) {
	t.Track(EventMessageReceived, hashID(chatID), map[string]interface{}{
		"device_id":    deviceID,
		"chat_id":      hashID(chatID),
		"message_type": msgType,
		"push_name":    pushName,
		"timestamp":    time.Now().Unix(),
	})
}

// TrackMessageTypeFiltered is called when a message is dropped before intent
// processing (e.g. group/community/channel/echo guard).
func (t *Tracker) TrackMessageTypeFiltered(deviceID, chatID, reason string) {
	t.Track(EventMessageTypeFiltered, hashID(chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"reason":    reason,
		"timestamp": time.Now().Unix(),
	})
}

// TrackIntentClassified records every intent classification result.
func (t *Tracker) TrackIntentClassified(deviceID, chatID, intentResult string, fromCache bool) {
	t.Track(EventIntentClassified, hashID(chatID), map[string]interface{}{
		"device_id":  deviceID,
		"chat_id":    hashID(chatID),
		"intent":     intentResult,
		"from_cache": fromCache,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackBusinessIntent records that a conversation was identified as business.
func (t *Tracker) TrackBusinessIntent(deviceID, chatID string) {
	t.Track(EventBusinessIntent, hashID(chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"timestamp": time.Now().Unix(),
	})
}

// TrackPersonalIntent records that a conversation was identified as personal.
func (t *Tracker) TrackPersonalIntent(deviceID, chatID string) {
	t.Track(EventPersonalIntent, hashID(chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"timestamp": time.Now().Unix(),
	})
}

// TrackUnclearIntent records that the intent could not be determined.
func (t *Tracker) TrackUnclearIntent(deviceID, chatID string) {
	t.Track(EventUnclearIntent, hashID(chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"timestamp": time.Now().Unix(),
	})
}

// TrackAIReplySent records a forwarded message (webhook sent to Python backend).
func (t *Tracker) TrackAIReplySent(deviceID, chatID, msgID, msgType string) {
	t.Track(EventAIReplySent, hashID(chatID), map[string]interface{}{
		"device_id":    deviceID,
		"chat_id":      hashID(chatID),
		"message_id":   msgID,
		"message_type": msgType,
		"timestamp":    time.Now().Unix(),
	})
}

// TrackAIReplySkipped records that an incoming message was NOT forwarded to the
// Python backend (personal or unclear intent).
func (t *Tracker) TrackAIReplySkipped(deviceID, chatID, msgID, reason string) {
	t.Track(EventAIReplySkipped, hashID(chatID), map[string]interface{}{
		"device_id":  deviceID,
		"chat_id":    hashID(chatID),
		"message_id": msgID,
		"reason":     reason,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackWebhookError records a webhook delivery failure.
func (t *Tracker) TrackWebhookError(deviceID, msgID string, err error) {
	t.Track(EventWebhookError, deviceID, map[string]interface{}{
		"device_id":  deviceID,
		"message_id": msgID,
		"error":      err.Error(),
		"timestamp":  time.Now().Unix(),
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal – HTTP delivery
// ─────────────────────────────────────────────────────────────────────────────

type capturePayload struct {
	APIKey     string                 `json:"api_key"`
	Event      string                 `json:"event"`
	DistinctID string                 `json:"distinct_id"`
	Properties map[string]interface{} `json:"properties"`
	Timestamp  string                 `json:"timestamp"`
}

func (t *Tracker) send(event, distinctID string, properties map[string]interface{}) {
	payload := capturePayload{
		APIKey:     t.apiKey,
		Event:      event,
		DistinctID: distinctID,
		Properties: properties,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("[analytics] failed to marshal PostHog payload", "event", event, "error", err)
		return
	}

	url := t.host + capturePath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("[analytics] failed to build PostHog request", "event", event, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		slog.Warn("[analytics] PostHog request failed", "event", event, "error", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("[analytics] PostHog returned non-2xx status",
			"event", event,
			"status", resp.StatusCode,
		)
	} else {
		slog.Debug("[analytics] PostHog event tracked", "event", event)
	}
}

// hashID returns a one-way pseudonym for a chat ID so no raw phone numbers
// are ever transmitted to PostHog. Uses a simple FNV-like hex representation.
// This is intentionally NOT cryptographically strong — it just prevents
// accidental PII exposure in PostHog dashboards.
func hashID(id string) string {
	if id == "" {
		return "unknown"
	}
	var h uint64 = 14695981039346656037
	for i := 0; i < len(id); i++ {
		h ^= uint64(id[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%x", h)
}
