// Package analytics provides a lightweight, non-blocking PostHog event tracker.
//
// Events are submitted fire-and-forget in a background goroutine so they never
// block the WhatsApp message processing pipeline. If PostHog is not configured
// (no API key) every method is a no-op.
package analytics

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	defaultHost   = "https://app.posthog.com"
	capturePath   = "/capture/"
	identifyPath  = "/capture/" // PostHog identify uses the same endpoint with event=$identify
	clientTimeout = 5 * time.Second
)

// ─────────────────────────────────────────────────────────────────────────────
// Event name constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// Message pipeline events
	EventMessageReceived     = "whatsmeow_message_received"
	EventMessageTypeFiltered = "whatsmeow_message_type_filtered"
	EventIntentClassified    = "whatsmeow_intent_classified"
	EventBusinessIntent      = "whatsmeow_business_intent_detected"
	EventPersonalIntent      = "whatsmeow_personal_intent_detected"
	EventUnclearIntent       = "whatsmeow_unclear_intent_detected"
	EventAIReplySent         = "whatsmeow_ai_reply_sent"
	EventAIReplySkipped      = "whatsmeow_ai_reply_skipped"
	EventWebhookError        = "whatsmeow_webhook_error"

	// Session lifecycle events
	EventQRInitiated        = "whatsmeow_qr_initiated"
	EventPairCodeGenerated  = "whatsmeow_pair_code_generated"
	EventSessionPaired      = "whatsmeow_session_paired"
	EventSessionConnected   = "whatsmeow_session_connected"
	EventSessionDisconnected = "whatsmeow_session_disconnected"
	EventSessionLoggedOut   = "whatsmeow_session_logged_out"
	EventSessionReconnected = "whatsmeow_session_reconnected"
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

// distinctID returns the canonical PostHog distinct_id for a customer message.
// Format: "biz:{deviceID}:cust:{sha256_16_of_chatID}"
//
// This MUST match the Python backend's posthog_client.distinct_id() so that
// events from the Go bridge and the Python backend merge onto the same PostHog
// Person record and funnels work end-to-end.
func (t *Tracker) distinctID(deviceID, chatID string) string {
	return fmt.Sprintf("biz:%s:cust:%s", deviceID, hashID(chatID))
}

// Track fires a PostHog event asynchronously. Errors are logged at WARN level
// but never propagate to the caller — message processing must never be blocked
// by analytics failures.
func (t *Tracker) Track(event, distinctID string, properties map[string]interface{}) {
	if !t.enabled {
		return
	}
	go t.send(event, distinctID, properties)
}

// Identify links a distinctID to a PostHog Person record with properties.
// This is required for events to appear in PostHog Insights, Persons, and
// Funnels (raw events always show in Activity regardless).
// Call this once per customer×business pair. Never raises.
func (t *Tracker) Identify(deviceID, chatID string, personProps map[string]interface{}) {
	if !t.enabled {
		return
	}
	did := t.distinctID(deviceID, chatID)
	props := make(map[string]interface{})
	for k, v := range personProps {
		props[k] = v
	}
	// PostHog identify: send $identify event with $set for person properties.
	go t.send("$identify", did, map[string]interface{}{
		"$set": props,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Convenience methods — message pipeline
// ─────────────────────────────────────────────────────────────────────────────

// TrackMessageReceived is called when a new inbound message arrives at the bridge.
func (t *Tracker) TrackMessageReceived(deviceID, chatID, msgType, pushName string) {
	did := t.distinctID(deviceID, chatID)
	t.Identify(deviceID, chatID, map[string]interface{}{
		"device_id": deviceID,
	})
	t.Track(EventMessageReceived, did, map[string]interface{}{
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
	t.Track(EventMessageTypeFiltered, t.distinctID(deviceID, chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"reason":    reason,
		"timestamp": time.Now().Unix(),
	})
}

// TrackIntentClassified records every intent classification result.
func (t *Tracker) TrackIntentClassified(deviceID, chatID, intentResult string, fromCache bool) {
	t.Track(EventIntentClassified, t.distinctID(deviceID, chatID), map[string]interface{}{
		"device_id":  deviceID,
		"chat_id":    hashID(chatID),
		"intent":     intentResult,
		"from_cache": fromCache,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackBusinessIntent records that a conversation was identified as business.
func (t *Tracker) TrackBusinessIntent(deviceID, chatID string) {
	t.Track(EventBusinessIntent, t.distinctID(deviceID, chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"timestamp": time.Now().Unix(),
	})
}

// TrackPersonalIntent records that a conversation was identified as personal.
func (t *Tracker) TrackPersonalIntent(deviceID, chatID string) {
	t.Track(EventPersonalIntent, t.distinctID(deviceID, chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"timestamp": time.Now().Unix(),
	})
}

// TrackUnclearIntent records that the intent could not be determined.
func (t *Tracker) TrackUnclearIntent(deviceID, chatID string) {
	t.Track(EventUnclearIntent, t.distinctID(deviceID, chatID), map[string]interface{}{
		"device_id": deviceID,
		"chat_id":   hashID(chatID),
		"timestamp": time.Now().Unix(),
	})
}

// TrackAIReplySent records a forwarded message (webhook sent to Python backend).
func (t *Tracker) TrackAIReplySent(deviceID, chatID, msgID, msgType string) {
	t.Track(EventAIReplySent, t.distinctID(deviceID, chatID), map[string]interface{}{
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
	t.Track(EventAIReplySkipped, t.distinctID(deviceID, chatID), map[string]interface{}{
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
// Convenience methods — session lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// TrackQRInitiated fires when the bridge starts a QR-code pairing session.
func (t *Tracker) TrackQRInitiated(sessionID string) {
	t.Track(EventQRInitiated, sessionID, map[string]interface{}{
		"session_id": sessionID,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackPairCodeGenerated fires when a phone-number pairing code is generated.
func (t *Tracker) TrackPairCodeGenerated(sessionID string) {
	t.Track(EventPairCodeGenerated, sessionID, map[string]interface{}{
		"session_id": sessionID,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackSessionPaired fires when a QR or pair-code pairing completes successfully.
func (t *Tracker) TrackSessionPaired(sessionID, method string) {
	t.Track(EventSessionPaired, sessionID, map[string]interface{}{
		"session_id": sessionID,
		"method":     method, // "qr" or "pair_code"
		"timestamp":  time.Now().Unix(),
	})
}

// TrackSessionConnected fires when a WhatsApp session comes online.
func (t *Tracker) TrackSessionConnected(sessionID string) {
	t.Track(EventSessionConnected, sessionID, map[string]interface{}{
		"session_id": sessionID,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackSessionDisconnected fires when a WhatsApp session goes offline.
func (t *Tracker) TrackSessionDisconnected(sessionID string) {
	t.Track(EventSessionDisconnected, sessionID, map[string]interface{}{
		"session_id": sessionID,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackSessionLoggedOut fires when a WhatsApp session is explicitly logged out
// or force-revoked by WhatsApp.
func (t *Tracker) TrackSessionLoggedOut(sessionID, reason string) {
	t.Track(EventSessionLoggedOut, sessionID, map[string]interface{}{
		"session_id": sessionID,
		"reason":     reason,
		"timestamp":  time.Now().Unix(),
	})
}

// TrackSessionReconnected fires when an existing paired session reconnects.
func (t *Tracker) TrackSessionReconnected(sessionID string) {
	t.Track(EventSessionReconnected, sessionID, map[string]interface{}{
		"session_id": sessionID,
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

// hashID returns a one-way pseudonym for a chat ID / phone number so no raw
// PII is transmitted to PostHog dashboards.
//
// Algorithm: SHA-256, first 8 bytes (= 16 hex chars).
// MUST match Python backend's posthog_client._hash_phone():
//   digest = hashlib.sha256(phone.encode("utf-8")).hexdigest()
//   return digest[:16]
func hashID(id string) string {
	if id == "" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:8]) // 8 bytes = 16 hex chars
}
