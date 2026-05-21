// Package client manages a pool of Whatsmeow clients, one per session / device ID.
// It handles:
//   - Session initialisation from SQLite stores
//   - QR-code and phone-number (pair-code) pairing
//   - Automatic reconnection with exponential back-off
//   - Incoming message event routing → webhook.Sender
//   - Inbound media download & local storage
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	qrterminal "github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"whatsapp-bridge/analytics"
	"whatsapp-bridge/intent"
	"whatsapp-bridge/store"
	"whatsapp-bridge/webhook"
)

// bridgeStartTime is recorded once when the package is loaded.
// It is used to skip offline-queue replay messages that WhatsApp pushes back
// to a newly (re)connected device — those messages were already processed in
// the previous session and must not be processed again.
var bridgeStartTime = time.Now()

// Status values returned by GetStatus.
const (
	StatusConnected    = "connected"
	StatusDisconnected = "disconnected"
	StatusConnecting   = "connecting"
	StatusNeedsPairing = "needs_pairing"
)

var errPairingReadyTimeout = errors.New("timed out waiting for pairing-ready websocket state")

// min returns the minimum of two integers (helper for string truncation in logs).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// PairingStateError means the caller asked to generate a pair code for a session
// that already has persisted linked-device credentials and must reconnect instead.
type PairingStateError struct {
	SessionID string
	Phone     string
	Status    string
}

func (e *PairingStateError) Error() string {
	if e == nil {
		return "session already paired"
	}
	if e.Phone != "" {
		return fmt.Sprintf("session %q is already paired to %s; reconnect instead of pairing", e.SessionID, e.Phone)
	}
	return fmt.Sprintf("session %q is already paired; reconnect instead of pairing", e.SessionID)
}

// session bundles a whatsmeow client with its bookkeeping state.
type session struct {
	id        string
	client    *whatsmeow.Client
	container *sqlstore.Container

	mu     sync.RWMutex
	status string
	opMu   sync.Mutex

	// cancelQR cancels the context used for the QR-code channel so that the
	// goroutine can be stopped when a pair-code flow takes over.
	cancelQR context.CancelFunc

	// qrMu protects qrPayload and qrReadyCh.
	qrMu sync.RWMutex
	// qrPayload holds the most recent QR payload string produced by the active
	// QR channel. Updated from connectWithQR on every "code" event. Empty when
	// no QR session is running.
	qrPayload string
	// qrReadyCh is closed when the first QR payload of the current QR session
	// becomes available. Re-created each time connectWithQR starts a new QR
	// channel so that GetQRPayload can block until a payload is ready.
	qrReadyCh chan struct{}

	// initializedInProcess is true when this session object was created by the
	// current process's EnsureSession call. It is used by the stale-DB self-heal
	// in GeneratePairCode to distinguish a freshly initialized (but not yet
	// paired) session from a leftover DB file written by a previous process.
	// The stale-DB purge must NOT fire for a fresh session — its key material is
	// valid and its SQLite file was just created this run; purging it would
	// destroy good keys and cause WhatsApp to reject the subsequent PairPhone.
	initializedInProcess bool
}

func (s *session) phone() string {
	if s.client.Store.ID == nil {
		return ""
	}
	return s.client.Store.ID.User
}

func (s *session) paired() bool {
	return s.client.Store.ID != nil
}

func (s *session) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

func (s *session) getStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// Manager owns all active sessions and wires incoming messages to the webhook sender.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*session

	// callMu protects the active call tracker.
	// Key: "<sessionID>:<callID>" — unique per session + call to avoid cross-session collisions.
	callMu sync.Mutex
	calls  map[string]*callEntry

	dbDir     string
	mediaDir  string
	publicURL string
	sender    *webhook.Sender

	// defaultSessionID is the global onboarding session (e.g. "smba").
	// Messages arriving on this session bypass the intent classifier so every
	// first-contact customer message reaches the Python onboarding service,
	// regardless of whether it matches a business/personal keyword pattern.
	defaultSessionID string

	// intentStore caches per-chat business/personal classification so the
	// bridge avoids re-classifying every message in an already-known thread.
	intentStore *intent.StateStore

	// tracker is the PostHog analytics client. It is always non-nil; when
	// PostHog is unconfigured the tracker is a no-op.
	tracker *analytics.Tracker
}

// callEntry tracks a single active incoming call and its auto-reject timer.
type callEntry struct {
	cancel    context.CancelFunc // cancels the 20 s reject-timer goroutine
	callFrom  types.JID          // caller's non-AD JID (pre-computed for fast access)
	callID    string
	sessionID string
	ownPhone  string // bridge's own WhatsApp number — included in the missed-call webhook
}

// NewManager creates an empty Manager.
// defaultSessionID is the global onboarding session ID (e.g. "smba");
// messages on that session bypass the intent classifier so all customer
// first-contact messages reach the Python onboarding service.
func NewManager(dbDir, mediaDir, publicURL string, sender *webhook.Sender, intentStore *intent.StateStore, tracker *analytics.Tracker, defaultSessionID string) *Manager {
	return &Manager{
		sessions:         make(map[string]*session),
		calls:            make(map[string]*callEntry),
		dbDir:            dbDir,
		mediaDir:         mediaDir,
		publicURL:        publicURL,
		sender:           sender,
		defaultSessionID: defaultSessionID,
		intentStore:      intentStore,
		tracker:          tracker,
	}
}

// Close gracefully disconnects active WhatsApp clients and closes backing stores.
// It is safe to call during process shutdown.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, s := range m.sessions {
		s.mu.Lock()
		if s.cancelQR != nil {
			s.cancelQR()
			s.cancelQR = nil
		}
		s.mu.Unlock()

		s.client.Disconnect()
		if err := s.container.Close(); err != nil {
			slog.Warn("failed to close session store", "session", id, "error", err)
		}
	}

	m.callMu.Lock()
	for key, entry := range m.calls {
		entry.cancel()
		delete(m.calls, key)
	}
	m.callMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// LoadExisting scans dbDir for *.db files and initialises a connected client for
// each found session. Call this once at startup.
func (m *Manager) LoadExisting(ctx context.Context) {
	entries, err := os.ReadDir(m.dbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		slog.Warn("cannot scan db dir", "error", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".db" {
			continue
		}
		sessionID := strings.TrimSuffix(e.Name(), ".db")
		slog.Info("loading existing session", "session_id", sessionID)
		if _, err := m.EnsureSession(ctx, sessionID); err != nil {
			slog.Error("failed to load session", "session_id", sessionID, "error", err)
		}
	}
}

// EnsureSession returns an existing session or initialises a new one.
func (m *Manager) EnsureSession(ctx context.Context, sessionID string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[sessionID]; ok {
		slog.Debug("[QR_DEBUG] EnsureSession: session already exists", "session", sessionID, "status", s.getStatus())
		return s, nil
	}

	slog.Info("[QR_DEBUG] EnsureSession: creating new session", "session", sessionID)
	container, err := store.NewContainer(sessionID, m.dbDir)
	if err != nil {
		slog.Error("[QR_ERROR] EnsureSession: Failed to create store container",
			"session", sessionID,
			"error", err)
		return nil, fmt.Errorf("store: %w", err)
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		slog.Error("[QR_ERROR] EnsureSession: Failed to get device from container",
			"session", sessionID,
			"error", err)
		return nil, fmt.Errorf("get device: %w", err)
	}

	logger := waLog.Stdout("WA-"+sessionID, "WARN", true)
	wac := whatsmeow.NewClient(device, logger)

	s := &session{
		id:                   sessionID,
		client:               wac,
		container:            container,
		status:               StatusDisconnected,
		initializedInProcess: true,
	}
	m.sessions[sessionID] = s

	// Register the event handler before connecting.
	wac.AddEventHandler(m.makeEventHandler(s))

	if wac.Store.ID == nil {
		// New device – print QR code to terminal so startup is usable immediately.
		slog.Info("[QR_DEBUG] EnsureSession: Session is unpaired, starting QR flow", "session", sessionID)
		go m.connectWithQR(s)
	} else {
		// Existing session – reconnect.
		slog.Info("[QR_DEBUG] EnsureSession: Session is paired, starting reconnect",
			"session", sessionID,
			"phone", s.phone())
		go m.connectWithRetry(s)
	}

	return s, nil
}

// GetClient returns the raw whatsmeow.Client for a session (must already exist).
func (m *Manager) GetClient(sessionID string) (*whatsmeow.Client, error) {
	m.mu.RLock()
	s, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	return s.client, nil
}

// GetStatus returns (status, phone, paired) for a session, or ("disconnected","",false) if unknown.
func (m *Manager) GetStatus(sessionID string) (status, phone string, paired bool) {
	m.mu.RLock()
	s, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return StatusDisconnected, "", false
	}
	st := s.getStatus()
	ph := s.phone()
	paired = s.paired()
	if !paired && st != StatusConnecting {
		st = StatusNeedsPairing
	}
	return st, ph, paired
}

// SessionExists reports whether a session is currently loaded in the pool.
// Returns false for sessions that were never paired or whose DB file is missing.
func (m *Manager) SessionExists(sessionID string) bool {
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	return ok
}

// ListSessions returns a snapshot of all loaded sessions and their status/phone.
func (m *Manager) ListSessions() []map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]map[string]string, 0, len(m.sessions))
	for id, s := range m.sessions {
		phone := s.phone()
		status := s.getStatus()
		if !s.paired() && status != StatusConnecting {
			status = StatusNeedsPairing
		}
		out = append(out, map[string]string{
			"sessionId": id,
			"status":    status,
			"phone":     phone,
		})
	}
	return out
}

// GeneratePairCode pairs a (possibly new) session using phone number instead of QR.
// Returns the 8-char code the user enters in WhatsApp → Linked Devices.
//
// Self-healing: if the session is not paired but a stale SQLite DB file exists
// on disk (left behind after a force-logout), GeneratePairCode purges it and
// re-initialises a genuinely fresh client rather than failing with a 400
// bad-request / privacy-token error from WhatsApp.  Callers must NOT call
// LogoutSession as a pre-pair reset; this function handles that internally.
func (m *Manager) GeneratePairCode(ctx context.Context, sessionID, phoneNumber string) (string, error) {
	slog.Info("GeneratePairCode: request started",
		"session", sessionID,
		"phone_masked", phoneNumber[:len(phoneNumber)-4]+"****",
	)

	s, err := m.EnsureSession(ctx, sessionID)
	if err != nil {
		slog.Error("GeneratePairCode: EnsureSession failed", "session", sessionID, "error", err)
		return "", err
	}

	slog.Info("GeneratePairCode: session ensured",
		"session", sessionID,
		"paired", s.paired(),
		"initialized_in_process", s.initializedInProcess,
		"status", s.getStatus(),
	)

	// ── Fast path: already paired ────────────────────────────────────────────
	// Return PairingStateError so the caller knows to reconnect instead.
	if s.paired() {
		if s.getStatus() != StatusConnected {
			slog.Info("GeneratePairCode: session already paired, triggering reconnect",
				"session", sessionID, "phone", s.phone())
			go m.connectWithRetry(s)
		}
		slog.Info("GeneratePairCode: returning PairingStateError (already paired)",
			"session", sessionID, "phone", s.phone(), "status", s.getStatus())
		return "", &PairingStateError{
			SessionID: sessionID,
			Phone:     s.phone(),
			Status:    s.getStatus(),
		}
	}

	// ── Stale-DB self-heal ────────────────────────────────────────────────────
	// A force-logout (events.LoggedOut) causes whatsmeow to call Store.Delete()
	// internally, which clears Store.ID (paired() → false) but leaves the SQLite
	// file on disk.  The file may contain orphaned prekey or identity-key rows.
	// If we let PairPhone run against this stale client/container, WhatsApp
	// rejects it with a 400 bad-request (privacy-token / prekey mismatch) that
	// surfaces as a 500 from our bridge.
	//
	// Detection: paired()==false AND the DB file exists on disk AND the session
	// was NOT initialized by the current process (i.e. it is a leftover from a
	// previous run). We must NOT purge a fresh session — its SQLite file was
	// just created this run with valid key material. Purging it on a resend
	// attempt would destroy valid keys, cause WhatsApp to reject the subsequent
	// PairPhone, and produce an intermittent 500.
	//
	// This replaces the Python-side "pre-pair logout" workaround — callers
	// should NOT call LogoutSession before GeneratePairCode.
	if !s.paired() && !s.initializedInProcess {
		dbPath := filepath.Join(m.dbDir, sessionID+".db")
		slog.Info("GeneratePairCode: checking for stale DB (session not paired and not from current process)",
			"session", sessionID, "db_path", dbPath)
		if _, statErr := os.Stat(dbPath); statErr == nil {
			// Stale DB from a previous process detected: purge and recreate fresh.
			slog.Warn("GeneratePairCode: stale DB from previous process detected – purging for clean re-pair",
				"session", sessionID, "db_path", dbPath)
			s.client.Disconnect()
			m.mu.Lock()
			delete(m.sessions, sessionID)
			m.mu.Unlock()
			m.deleteSessionDB(sessionID)
			// Re-initialise: new container, new whatsmeow.Client, fresh key material.
			s, err = m.EnsureSession(ctx, sessionID)
			if err != nil {
				slog.Error("GeneratePairCode: re-init session after stale-DB purge failed",
					"session", sessionID, "error", err)
				return "", fmt.Errorf("re-init session after stale-DB purge: %w", err)
			}
			slog.Info("GeneratePairCode: stale DB purge complete, proceeding with fresh session",
				"session", sessionID)
		} else {
			slog.Debug("GeneratePairCode: no stale DB file found", "session", sessionID, "error", statErr)
		}
	} else {
		slog.Info("GeneratePairCode: skipping stale-DB check",
			"session", sessionID,
			"paired", s.paired(),
			"initialized_in_process", s.initializedInProcess)
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()

	// Re-check after acquiring the lock in case a concurrent goroutine paired
	// this session between our check above and acquiring opMu.
	if s.paired() {
		slog.Info("GeneratePairCode: session paired after acquiring opMu",
			"session", sessionID, "phone", s.phone())
		return "", &PairingStateError{
			SessionID: sessionID,
			Phone:     s.phone(),
			Status:    s.getStatus(),
		}
	}

	// Cancel any running QR goroutine first.
	s.mu.Lock()
	if s.cancelQR != nil {
		slog.Debug("GeneratePairCode: canceling existing QR goroutine", "session", sessionID)
		s.cancelQR()
		s.cancelQR = nil
	}
	s.mu.Unlock()

	// Always disconnect and reconnect fresh for the pair-code flow.
	//
	// Why: EnsureSession starts connectWithQR in a background goroutine which
	// may have already called Connect() and left the websocket open in QR-auth
	// mode.  Calling PairPhone on that connection makes WhatsApp reject it.
	// A clean Disconnect → Connect gives us an unauthenticated websocket that
	// PairPhone can use correctly. We also wait for the QR/pairing state to be
	// emitted before calling PairPhone, as required by WhatsMeow's pre-login flow.
	slog.Info("GeneratePairCode: disconnecting for fresh pair-code flow", "session", sessionID)
	s.client.Disconnect()
	time.Sleep(500 * time.Millisecond) // let the QR goroutine fully exit

	readyCh := make(chan error, 1)
	handlerID := s.client.AddEventHandler(func(raw interface{}) {
		switch evt := raw.(type) {
		case *events.QR:
			slog.Debug("GeneratePairCode: received QR event", "session", sessionID)
			select {
			case readyCh <- nil:
			default:
				slog.Debug("GeneratePairCode: readyCh already signaled (QR event)", "session", sessionID)
			}
		case *events.Connected:
			slog.Info("GeneratePairCode: received Connected event during pair-code flow (unexpected)",
				"session", sessionID)
			select {
			case readyCh <- &PairingStateError{SessionID: sessionID, Phone: s.phone(), Status: StatusConnected}:
			default:
				slog.Debug("GeneratePairCode: readyCh already signaled (Connected event)", "session", sessionID)
			}
		case *events.LoggedOut:
			slog.Error("GeneratePairCode: received LoggedOut event during pair-code flow",
				"session", sessionID)
			select {
			case readyCh <- errors.New("pairing websocket was logged out during pre-login setup"):
			default:
				slog.Debug("GeneratePairCode: readyCh already signaled (LoggedOut event)", "session", sessionID)
			}
		default:
			slog.Debug("GeneratePairCode: ignoring non-pairing event",
				"session", sessionID,
				"event_type", fmt.Sprintf("%T", evt))
		}
	})
	defer s.client.RemoveEventHandler(handlerID)

	slog.Info("GeneratePairCode: event handler registered, calling Connect", "session", sessionID)
	s.setStatus(StatusConnecting)
	connectErr := s.client.Connect()
	if connectErr != nil && connectErr != whatsmeow.ErrAlreadyConnected {
		s.setStatus(StatusDisconnected)
		slog.Error("GeneratePairCode: Connect failed",
			"session", sessionID,
			"error", connectErr)
		return "", fmt.Errorf("connect for pair-code: %w", connectErr)
	}
	slog.Debug("GeneratePairCode: Connect completed successfully", "session", sessionID)

	slog.Info("GeneratePairCode: waiting for QR/pairing ready event (timeout 20s)",
		"session", sessionID)
	select {
	case readyErr := <-readyCh:
		if readyErr != nil {
			var pairingErr *PairingStateError
			if errors.As(readyErr, &pairingErr) {
				slog.Info("GeneratePairCode: received PairingStateError during ready wait",
					"session", sessionID, "error", readyErr)
				s.setStatus(StatusConnected)
			} else {
				slog.Warn("GeneratePairCode: received error during ready wait",
					"session", sessionID, "error", readyErr)
				s.setStatus(StatusNeedsPairing)
			}
			return "", readyErr
		}
		slog.Debug("GeneratePairCode: received ready event (QR code ready)", "session", sessionID)
	case <-ctx.Done():
		s.setStatus(StatusNeedsPairing)
		slog.Error("GeneratePairCode: context cancelled before pairing ready",
			"session", sessionID, "error", ctx.Err())
		return "", ctx.Err()
	case <-time.After(20 * time.Second):
		s.setStatus(StatusNeedsPairing)
		slog.Error("GeneratePairCode: timeout waiting for pairing ready event (20s exceeded)",
			"session", sessionID)
		return "", errPairingReadyTimeout
	}

	// Strip any non-digit characters from the phone number.
	digits := digitsOnly(phoneNumber)
	slog.Info("GeneratePairCode: calling PairPhone",
		"session", sessionID,
		"phone_digits_len", len(digits),
		"initialized_in_process", s.initializedInProcess,
	)
	code, err := s.client.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		slog.Error("GeneratePairCode: PairPhone failed",
			"session", sessionID,
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
			"initialized_in_process", s.initializedInProcess,
			"paired", s.paired(),
		)
		s.setStatus(StatusNeedsPairing)
		return "", fmt.Errorf("pair-phone: %w", err)
	}
	slog.Info("GeneratePairCode: PairPhone succeeded, returning code",
		"session", sessionID,
		"code_len", len(code),
	)
	return code, nil
}

// deleteSessionDB removes the SQLite database and its WAL/SHM sidecar files
// for sessionID. Safe to call when files do not exist (os.IsNotExist is
// silently ignored). Non-existence errors are logged at WARN level.
func (m *Manager) deleteSessionDB(sessionID string) {
	for _, suffix := range []string{".db", ".db-wal", ".db-shm"} {
		path := filepath.Join(m.dbDir, sessionID+suffix)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove session DB file", "path", path, "error", err)
		}
	}
}

// ReconnectSession ensures a stored linked-device session is connected again.
func (m *Manager) ReconnectSession(ctx context.Context, sessionID string) error {
	s, err := m.EnsureSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !s.paired() {
		return fmt.Errorf("session %q requires pairing before it can reconnect", sessionID)
	}
	go m.connectWithRetry(s)
	return nil
}

// LogoutSession is for EXPLICIT USER-INITIATED unlinks only (e.g. the owner
// says "disconnect my WhatsApp"). It revokes the WhatsApp linked-device auth,
// removes the session from the in-memory pool, and deletes the SQLite DB files
// so the next pairing attempt starts from a completely clean state.
//
// Do NOT call LogoutSession for transient disconnects — those are handled by
// connectWithRetry automatically.  Do NOT call LogoutSession as a pre-pair
// reset before GeneratePairCode — GeneratePairCode self-heals stale DBs
// internally and calling LogoutSession on a healthy session would destroy valid
// credentials unnecessarily.
//
// Guard: if the session currently shows StatusConnected this function still
// proceeds (the caller explicitly requested an unlink), but logs a warning so
// accidental calls are visible in production logs.
func (m *Manager) LogoutSession(ctx context.Context, sessionID string) error {
	// Remove from the pool under a write lock.
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if ok {
		currentStatus := s.getStatus()
		if currentStatus == StatusConnected {
			// This is unusual — log so accidental calls in prod are visible.
			slog.Warn("LogoutSession called on a currently-connected session; proceeding with explicit unlink",
				"session", sessionID)
		}
		if s.paired() {
			// Revoke linked-device auth server-side.  A network error is non-fatal
			// — we always clean up locally regardless.
			if err := s.client.Logout(ctx); err != nil {
				slog.Warn("WhatsApp API logout returned error (continuing with local cleanup)",
					"session", sessionID, "error", err)
			}
		}
		s.client.Disconnect()
		s.setStatus(StatusNeedsPairing)
	}

	// Always delete the SQLite store files.  This is intentional for an explicit
	// unlink: it guarantees the next pairing flow uses fresh identity keys and
	// avoids WhatsApp rejecting the session with a 400 bad-request error.
	m.deleteSessionDB(sessionID)
	slog.Info("session explicitly logged out and DB purged", "session", sessionID)
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal – connection management
// ──────────────────────────────────────────────────────────────────────────────

func (m *Manager) connectWithQR(s *session) {
	slog.Info("[QR_DEBUG] Starting QR connection flow", "session", s.id)
	ctx, cancel := context.WithCancel(context.Background())

	// Create a fresh readyCh so that GetQRPayload callers can block until the
	// first QR payload is available.  Reset qrPayload to empty so that stale
	// payloads from a previous QR session are never served.
	readyCh := make(chan struct{})
	s.mu.Lock()
	s.cancelQR = cancel
	s.mu.Unlock()
	s.qrMu.Lock()
	s.qrPayload = ""
	s.qrReadyCh = readyCh
	s.qrMu.Unlock()

	slog.Debug("[QR_DEBUG] Attempting to open QR channel", "session", s.id)
	qrChan, err := s.client.GetQRChannel(ctx)
	if err != nil {
		slog.Error("[QR_ERROR] Failed to open QR channel", 
			"session", s.id, 
			"error", err,
			"error_type", fmt.Sprintf("%T", err))
		// Close readyCh first to unblock any GetQRPayload callers that have already
		// picked up this channel reference — otherwise they block for the full
		// 45-second timeout on a channel that will never receive a QR code.
		// Then nil out qrReadyCh so the next GetQRPayload call starts a fresh goroutine.
		close(readyCh)
		s.qrMu.Lock()
		s.qrReadyCh = nil
		s.qrMu.Unlock()
		cancel()
		return
	}

	s.setStatus(StatusConnecting)
	slog.Debug("[QR_DEBUG] Calling client.Connect()", "session", s.id)
	if err := s.client.Connect(); err != nil && err != whatsmeow.ErrAlreadyConnected {
		slog.Error("[QR_ERROR] Client.Connect() failed", 
			"session", s.id, 
			"error", err,
			"error_type", fmt.Sprintf("%T", err))
		s.setStatus(StatusDisconnected)
		// Same as above: unblock callers immediately and reset for the next attempt.
		close(readyCh)
		s.qrMu.Lock()
		s.qrReadyCh = nil
		s.qrMu.Unlock()
		cancel()
		return
	}

	firstCode := true
	for evt := range qrChan {
		slog.Debug("[QR_DEBUG] Received QR event", "session", s.id, "event", evt.Event)
		switch evt.Event {
		case "code":
			// Store the latest payload so polling callers can retrieve it.
			if evt.Code == "" {
				slog.Error("[QR_ERROR] Received empty QR code", "session", s.id)
			} else {
				slog.Info("[QR_DEBUG] New QR code generated", 
					"session", s.id, 
					"code_length", len(evt.Code),
					"code_prefix", evt.Code[:min(10, len(evt.Code))])
			}
			s.qrMu.Lock()
			s.qrPayload = evt.Code
			s.qrMu.Unlock()

			// Close readyCh once (first code) to unblock GetQRPayload waiters.
			if firstCode {
				firstCode = false
				slog.Info("[QR_DEBUG] First QR code ready, unblocking waiters", "session", s.id)
				close(readyCh)
			}

			fmt.Printf("\n[session: %s] Scan this QR code in WhatsApp → Linked Devices:\n", s.id)
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
		case "login":
			slog.Info("[QR_DEBUG] Login event received from WhatsApp", "session", s.id)
			s.setStatus(StatusConnecting)

			// Wait up to 30 seconds for the connection to be fully authenticated.
			// Increased from 20s to handle slower networks.
			slog.Debug("[QR_DEBUG] Waiting for connection to be fully established (30s max)", "session", s.id)
			if s.client.WaitForConnection(30 * time.Second) {
				slog.Info("[QR_SUCCESS] QR connection fully established", "session", s.id)
				s.setStatus(StatusConnected)
			} else {
				// Do NOT mark as connected on timeout — the handshake is incomplete.
				// connectWithRetry will re-establish the connection.
				slog.Error("[QR_ERROR] WaitForConnection timed out after login event — triggering reconnect",
					"session", s.id)
				s.setStatus(StatusDisconnected)
				go m.connectWithRetry(s)
			}
		case "timeout":
			// QR code expired without being scanned.  Clear the stale payload
			// immediately so that polling callers (GET /api/qr-current) never
			// return an expired code that WhatsApp will reject with
			// "we could not connect, try again later".
			slog.Warn("[QR_TIMEOUT] QR code expired without being scanned", "session", s.id)
			s.qrMu.Lock()
			s.qrPayload = ""
			s.qrMu.Unlock()
			s.setStatus(StatusDisconnected)
		default:
			slog.Debug("[QR_DEBUG] Unknown QR event received", "session", s.id, "event", evt.Event)
		}
	}
	slog.Warn("[QR_DEBUG] QR channel closed (loop exited)", "session", s.id)
	cancel()

	// Clear stale QR state so no expired payload is ever served after this
	// goroutine exits.  Reset qrReadyCh to nil so the next GetQRPayload call
	// correctly starts a fresh QR goroutine instead of instantly returning
	// the (now-gone) old channel.
	s.qrMu.Lock()
	s.qrPayload = ""
	s.qrReadyCh = nil
	s.qrMu.Unlock()

	// If the session is still unpaired the QR timed out without being scanned.
	// Auto-restart the QR flow after a brief pause so a fresh code is always
	// available without requiring manual intervention from the Python backend.
	if !s.paired() {
		slog.Info("[QR_DEBUG] QR session ended without pairing — scheduling QR restart in 2s", "session", s.id)
		time.Sleep(2 * time.Second)
		go m.connectWithQR(s)
	} else {
		slog.Info("[QR_DEBUG] Session is paired, not restarting QR flow", "session", s.id)
	}
}

// GetQRPayload ensures a QR session is running for sessionID and blocks until
// the first QR payload is available (or timeout fires).
//
// Returns (payload, nil) on success.
// Returns ("", PairingStateError) if the session is already paired.
// Returns ("", err) on any other failure or timeout.
func (m *Manager) GetQRPayload(ctx context.Context, sessionID string, timeout time.Duration) (string, error) {
	slog.Debug("[QR_DEBUG] GetQRPayload called", "session", sessionID, "timeout", timeout)
	s, err := m.EnsureSession(ctx, sessionID)
	if err != nil {
		slog.Error("[QR_ERROR] EnsureSession failed in GetQRPayload",
			"session", sessionID,
			"error", err)
		return "", fmt.Errorf("GetQRPayload: ensure session: %w", err)
	}

	// If already paired, return an error so the caller can reconnect instead.
	if s.paired() {
		slog.Info("[QR_DEBUG] Session already paired, returning PairingStateError",
			"session", sessionID,
			"phone", s.phone(),
			"status", s.getStatus())
		return "", &PairingStateError{SessionID: sessionID, Phone: s.phone(), Status: s.getStatus()}
	}

	// Grab the current readyCh; start a QR goroutine if none is running.
	s.qrMu.RLock()
	readyCh := s.qrReadyCh
	s.qrMu.RUnlock()

	if readyCh == nil {
		slog.Debug("[QR_DEBUG] No QR channel ready yet, waiting for goroutine initialization (1s timeout)", "session", sessionID)
		// EnsureSession starts connectWithQR in a goroutine; give it up to 1 s to
		// set qrReadyCh before we assume no goroutine is running.  A single 50 ms
		// sleep is too short on production (Go scheduler may not have run the
		// goroutine yet), which caused a second connectWithQR to be launched in
		// parallel — the two goroutines would race to overwrite qrReadyCh and one
		// would inevitably leave it pointing at an abandoned channel → timeout.
		deadline := time.Now().Add(1 * time.Second)
		attempts := 0
		for readyCh == nil && time.Now().Before(deadline) {
			attempts++
			time.Sleep(50 * time.Millisecond)
			s.qrMu.RLock()
			readyCh = s.qrReadyCh
			s.qrMu.RUnlock()
		}
		slog.Debug("[QR_DEBUG] Completed readyCh polling", "session", sessionID, "attempts", attempts, "channel_ready", readyCh != nil)

		if readyCh == nil {
			// EnsureSession may have started connectWithRetry (existing paired
			// session that just disconnected).  Launch a fresh QR goroutine.
			slog.Info("[QR_DEBUG] Launching manual QR connection goroutine", "session", sessionID)
			go m.connectWithQR(s)
			time.Sleep(200 * time.Millisecond)
			s.qrMu.RLock()
			readyCh = s.qrReadyCh
			s.qrMu.RUnlock()
			slog.Debug("[QR_DEBUG] After manual goroutine launch", "session", sessionID, "channel_ready", readyCh != nil)
		}
	}

	if readyCh == nil {
		slog.Error("[QR_ERROR] Could not start QR flow (readyCh is still nil after all attempts)", "session", sessionID)
		return "", errors.New("QR flow could not be started")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	slog.Debug("[QR_DEBUG] Waiting for QR payload to become available", 
		"session", sessionID, 
		"timeout", timeout)

	select {
	case <-readyCh:
		s.qrMu.RLock()
		payload := s.qrPayload
		s.qrMu.RUnlock()
		if payload == "" {
			slog.Error("[QR_ERROR] QR payload is empty after ready signal", "session", sessionID)
			return "", errors.New("QR payload is empty after ready signal")
		}
		slog.Info("[QR_SUCCESS] GetQRPayload returning QR payload", 
			"session", sessionID, 
			"payload_len", len(payload),
			"payload_prefix", payload[:min(10, len(payload))])
		return payload, nil
	case <-ctx.Done():
		slog.Error("[QR_ERROR] Context cancelled while waiting for QR payload", 
			"session", sessionID, 
			"error", ctx.Err())
		return "", ctx.Err()
	case <-timer.C:
		slog.Error("[QR_TIMEOUT] Timeout waiting for QR code to become available", 
			"session", sessionID, 
			"timeout", timeout)
		return "", errors.New("timeout waiting for QR code to become available")
	}
}

// GetCurrentQRPayload returns the most recent QR payload for sessionID without
// blocking.  Returns ("", false) when no active QR payload is stored.
func (m *Manager) GetCurrentQRPayload(sessionID string) (string, bool) {
	m.mu.RLock()
	s, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return "", false
	}
	s.qrMu.RLock()
	payload := s.qrPayload
	s.qrMu.RUnlock()
	if payload == "" {
		return "", false
	}
	return payload, true
}

func (m *Manager) connectWithRetry(s *session) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	if !s.paired() {
		s.setStatus(StatusNeedsPairing)
		return
	}

	delay := 2 * time.Second
	const maxDelay = 60 * time.Second

	for {
		s.setStatus(StatusConnecting)
		err := s.client.Connect()
		if err == nil || err == whatsmeow.ErrAlreadyConnected {
			if s.client.WaitForConnection(30 * time.Second) {
				s.setStatus(StatusConnected)
				return
			}
			err = errors.New("connection established but authentication did not complete in time")
			s.client.Disconnect()
		}

		if !s.paired() {
			s.setStatus(StatusNeedsPairing)
			return
		}

		slog.Warn("reconnect failed, retrying",
			"session", s.id,
			"delay", delay,
			"error", err,
		)
		s.setStatus(StatusDisconnected)
		time.Sleep(delay)
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal – event handling
// ──────────────────────────────────────────────────────────────────────────────

func (m *Manager) makeEventHandler(s *session) func(interface{}) {
	return func(raw interface{}) {
		switch evt := raw.(type) {
		case *events.Connected:
			s.setStatus(StatusConnected)
			slog.Info("WhatsApp connected", "session", s.id)

		case *events.Disconnected:
			s.setStatus(StatusDisconnected)
			slog.Info("WhatsApp disconnected", "session", s.id)

		case *events.LoggedOut:
			// WhatsApp has revoked this session's credentials server-side.
			// The in-memory whatsmeow client's Store.ID has been cleared by
			// whatsmeow's own cleanup, but the SQLite file may still contain
			// orphaned prekey / identity-key rows. If we leave the file and let
			// the same *whatsmeow.Client be reused, PairPhone will fail with a
			// 400 bad-request (privacy-token mismatch). Remove the session from
			// the pool and delete the DB files so the next EnsureSession creates
			// a brand-new client with genuinely fresh key material.
			slog.Warn("WhatsApp force-logout received – purging session and DB for clean re-pair",
				"session", s.id)
			s.client.Disconnect()
			s.setStatus(StatusNeedsPairing)
			m.mu.Lock()
			delete(m.sessions, s.id)
			m.mu.Unlock()
			m.deleteSessionDB(s.id)

		case *events.StreamReplaced:
			s.setStatus(StatusDisconnected)
			slog.Warn("WhatsApp stream replaced by another active client", "session", s.id)

		case *events.Message:
			m.handleIncoming(s, evt)

		// ── Call events ────────────────────────────────────────────────────
		// CallOffer:     incoming call — start a 20 s auto-reject timer.
		// CallTerminate: caller hung up / other side ended — cancel timer.
		// CallReject:    call was rejected elsewhere — cancel timer.
		// CallAccept:    call accepted on another linked device — cancel timer.
		case *events.CallOffer:
			m.handleCallOffer(s, evt)

		case *events.CallTerminate:
			m.handleCallEnd(s.id, evt.CallID, "terminated", evt.Reason)

		case *events.CallReject:
			m.handleCallEnd(s.id, evt.CallID, "rejected", "")

		case *events.CallAccept:
			m.handleCallEnd(s.id, evt.CallID, "accepted", "")
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal – call handling
// ──────────────────────────────────────────────────────────────────────────────

// handleCallOffer is called when an incoming WhatsApp call arrives.
// It starts a 20 s auto-reject timer; if the call is not terminated/rejected
// before the timer fires we reject it on behalf of the bridge and notify
// the Python backend so it can send a follow-up voice note.
func (m *Manager) handleCallOffer(s *session, evt *events.CallOffer) {
	callID := evt.CallID
	// Use the non-AD JID so RejectCall receives the same form it sends.
	callFrom := evt.From.ToNonAD()
	callerPhone := callFrom.User // raw digits, same convention as chat_id in messages

	ownPhone := ""
	if s.client.Store.ID != nil {
		ownPhone = s.client.Store.ID.User
	}

	slog.Info("[CALL] incoming call offer",
		"session", s.id,
		"call_id", callID,
		"caller_jid", callFrom.String(),
		"caller_phone", callerPhone,
	)

	// Send call_offer event to Python for observability / logging.
	go m.sender.SendCall(webhook.CallEvent{
		Event:    "call_offer",
		DeviceID: s.id,
		Phone:    ownPhone,
		Payload: webhook.CallPayload{
			CallID:      callID,
			CallerJID:   callFrom.String(),
			CallerPhone: callerPhone,
			PushName:    "",
			Timestamp:   evt.Timestamp.Unix(),
		},
	})

	// Dedup guard: ignore duplicate offer events for the same call.
	key := s.id + ":" + callID
	m.callMu.Lock()
	if _, exists := m.calls[key]; exists {
		m.callMu.Unlock()
		slog.Debug("[CALL] duplicate call offer ignored", "key", key)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &callEntry{
		cancel:    cancel,
		callFrom:  callFrom,
		callID:    callID,
		sessionID: s.id,
		ownPhone:  ownPhone,
	}
	m.calls[key] = entry
	m.callMu.Unlock()

	slog.Info("[CALL] 20 s auto-reject timer started",
		"session", s.id,
		"call_id", callID,
		"caller_phone", callerPhone,
	)

	go m.callTimeoutWorker(ctx, entry)
}

// callTimeoutWorker waits 20 seconds for the call to be resolved naturally.
// If the context is cancelled (call ended) it exits cleanly.
// If the timer fires it rejects the call and notifies Python.
func (m *Manager) callTimeoutWorker(ctx context.Context, entry *callEntry) {
	select {
	case <-ctx.Done():
		// The call was resolved (terminated / rejected / accepted) before timeout.
		slog.Info("[CALL] timer cancelled — call ended before 20 s",
			"session", entry.sessionID,
			"call_id", entry.callID,
		)
		return
	case <-time.After(20 * time.Second):
		// Timer expired — proceed to reject and notify.
	}

	// Remove from the tracker (timer goroutine "wins" the cleanup race).
	key := entry.sessionID + ":" + entry.callID
	m.callMu.Lock()
	_, stillActive := m.calls[key]
	if stillActive {
		delete(m.calls, key)
	}
	m.callMu.Unlock()

	if !stillActive {
		// handleCallEnd already cleaned this up — nothing to do.
		slog.Debug("[CALL] call already cleaned up before timer fired, skipping reject", "key", key)
		return
	}

	slog.Info("[CALL] 20 s timeout — rejecting call",
		"session", entry.sessionID,
		"call_id", entry.callID,
		"caller_jid", entry.callFrom.String(),
	)

	// Reject the call via WhatsMeow.
	wac, err := m.GetClient(entry.sessionID)
	if err != nil {
		slog.Error("[CALL] cannot get whatsmeow client to reject call",
			"session", entry.sessionID,
			"error", err,
		)
	} else {
		rejectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := wac.RejectCall(rejectCtx, entry.callFrom, entry.callID); err != nil {
			slog.Error("[CALL] RejectCall failed",
				"session", entry.sessionID,
				"call_id", entry.callID,
				"error", err,
			)
		} else {
			slog.Info("[CALL] call rejected successfully",
				"session", entry.sessionID,
				"call_id", entry.callID,
			)
		}
	}

	// Notify Python backend to send a follow-up voice note to the caller.
	slog.Info("[CALL] sending call_missed webhook",
		"session", entry.sessionID,
		"call_id", entry.callID,
		"caller_phone", entry.callFrom.User,
	)
	m.sender.SendCall(webhook.CallEvent{
		Event:    "call_missed",
		DeviceID: entry.sessionID,
		Phone:    entry.ownPhone,
		Payload: webhook.CallPayload{
			CallID:      entry.callID,
			CallerJID:   entry.callFrom.String(),
			CallerPhone: entry.callFrom.User,
			PushName:    "",
			Timestamp:   time.Now().Unix(),
		},
	})
	slog.Info("[CALL] call_missed webhook sent",
		"session", entry.sessionID,
		"call_id", entry.callID,
	)
}

// handleCallEnd is called when a tracked call terminates before the 20 s timer fires.
// It cancels the timer and removes the entry from the tracker.
func (m *Manager) handleCallEnd(sessionID, callID, reason, detail string) {
	key := sessionID + ":" + callID
	m.callMu.Lock()
	entry, exists := m.calls[key]
	if exists {
		entry.cancel() // cancel the 20 s timer goroutine
		delete(m.calls, key)
	}
	m.callMu.Unlock()

	if exists {
		slog.Info("[CALL] call ended before timeout (timer cancelled)",
			"session", sessionID,
			"call_id", callID,
			"reason", reason,
			"detail", detail,
		)
	}
}

func (m *Manager) handleIncoming(s *session, evt *events.Message) {
	info := evt.Info

	// ── Verbose entry log — always at INFO so production traces are clear ──────
	// Shows the key fields for every incoming event so the exact processing
	// path ("which handler", "which device", "which message_id") is always
	// traceable without enabling DEBUG mode.
	slog.Info("[bridge] incoming message",
		"session", s.id,
		"msg_id", info.ID,
		"from", info.Sender.ToNonAD().String(),
		"chat", info.Chat.User,
		"own_phone", s.phone(),
		"is_from_me", info.IsFromMe,
		"is_group", info.IsGroup,
	)

	// ── Guard 1: outbound-echo suppression (is_from_me=true) ─────────────────
	// When we call SendMessage() the WhatsApp server echoes it back as an
	// events.Message with IsFromMe=true.  The Python backend has no use for
	// these — drop them here so they never reach the webhook endpoint.
	if info.IsFromMe {
		slog.Debug("[bridge] skipping outbound echo (is_from_me=true)",
			"session", s.id, "id", info.ID, "chat", info.Chat.User)
		m.tracker.TrackMessageTypeFiltered(s.id, info.Chat.User, "outbound_echo")
		return
	}

	// ── Guard 1.5: self-chat echo suppression ─────────────────────────────────
	// Certain WhatsApp multi-device protocol versions deliver sent-message
	// sync events back to the sending device as regular incoming messages with
	// Chat.User == the device's own phone and IsFromMe == false.
	// This is a WhatsMeow quirk — the resulting webhook would look like a real
	// incoming customer message but is actually an echo of something we just
	// sent.  Processing it creates an AI → echo → AI feedback loop.
	// Telltale signature: non-group chat where chat.User == our own phone.
	if !info.IsGroup && s.phone() != "" && info.Chat.User == s.phone() {
		slog.Info("[bridge] skipping self-chat echo (chat.user == own phone)",
			"session", s.id,
			"msg_id", info.ID,
			"chat", info.Chat.User,
			"own_phone", s.phone(),
		)
		m.tracker.TrackMessageTypeFiltered(s.id, info.Chat.User, "self_chat_echo")
		return
	}

	// ── Guard 2: offline-queue replay suppression ────────────────────────────
	// On reconnect WhatsApp replays messages that arrived while the bridge was
	// down.  The Python dedup cache is in-memory and empty after restart, so
	// without this guard every replayed message would be reprocessed.
	// Grace window: 30 s before bridge start so messages arriving just before
	// a restart are still processed.
	const replayGrace = 30 * time.Second
	if info.Timestamp.Before(bridgeStartTime.Add(-replayGrace)) {
		slog.Info("[bridge] skipping offline-replay message (pre-startup)",
			"session", s.id,
			"id", info.ID,
			"from", info.Sender.String(),
			"age", time.Since(info.Timestamp).Round(time.Second),
		)
		m.tracker.TrackMessageTypeFiltered(s.id, info.Chat.User, "offline_replay")
		return
	}

	// Dispatch the heavy work (LID server-roundtrip, media download, webhook)
	// to a goroutine so whatsmeow's event loop is not blocked.  The guards
	// above are all cheap (no I/O) and must run synchronously so filtered
	// messages are accounted before we return to the loop.
	go func() {
		// Determine message type and body.
		msgType, body, mediaURL, mimeType := classifyMessage(s, m, evt)
		if msgType == "" {
			m.tracker.TrackMessageTypeFiltered(s.id, info.Chat.User, "unsupported_type")
			return // unsupported/ignored message type
		}

		// Sender's full JID used as the reply-to address in the webhook payload.
		// info.Sender is an AD-JID (Agent-Device JID) for 1:1 messages — e.g.
		// "35317885218956:74@lid" or "917696794756:12@s.whatsapp.net".
		// WhatsApp's /send/message endpoint only accepts user-level JIDs (no device
		// part), so we strip the device component with ToNonAD() before forwarding.
		// Result: "35317885218956@lid" or "917696794756@s.whatsapp.net".
		from := info.Sender.ToNonAD().String()

		// chat_id: the raw user-part of the chat JID (digits for normal phones, or
		// the LID user string for privacy-protected contacts).
		chatID := info.Chat.User

		// ── LID → Phone Number resolution ────────────────────────────────────────
		// WhatsApp privacy mode causes messages to arrive from @lid JIDs instead
		// of the sender's real phone JID.  Resolve using WhatsMeow's LID map which
		// is populated by WhatsApp contact-sync data.  If resolved:
		//   • senderPN  = full phone JID e.g. "917696794756@s.whatsapp.net"
		//   • chatID    = real phone digits (sent as chat_id to backend for DB ops)
		// If NOT resolved (new contact not yet in cache), we fall through with the
		// LID digits as chatID — the backend will use LID as a stable customer key
		// until a sync populates the mapping.
		senderPN := ""
		if info.Sender.Server == types.HiddenUserServer {
			ctx := context.Background()
			if pnJID, err := s.container.LIDMap.GetPNForLID(ctx, info.Sender); err == nil && !pnJID.IsEmpty() {
				senderPN = pnJID.String()
				// Use real phone digits as chat_id so backend DB operations bind
				// to the customer's stable phone number rather than the LID.
				chatID = pnJID.User
				slog.Info("[bridge] resolved LID to PN",
					"session", s.id,
					"lid", from,
					"pn", senderPN,
				)
			} else {
				slog.Debug("[bridge] LID not yet in cache — using LID digits as customer key",
					"session", s.id,
					"lid", from,
				)
			}
		}

		// Business phone number this session is registered to (our own WhatsApp number).
		ownPhone := ""
		if s.client.Store.ID != nil {
			ownPhone = s.client.Store.ID.User // raw digits
		}

		// Track that this message passed all guards and reached the AI pipeline.
		m.tracker.TrackMessageReceived(s.id, chatID, msgType, info.PushName)

		// -- Intent classification --------------------------------------------------
		// The global onboarding session (smba) handles ALL first-contact customer
		// messages regardless of content — the Python onboarding service needs to
		// see every message to drive the onboarding state machine.  Applying the
		// intent filter here would silently drop messages that don't match a
		// business keyword (e.g. "I run a restaurant in Lisbon") and break the flow.
		if m.defaultSessionID != "" && s.id == m.defaultSessionID {
			slog.Info("[bridge] onboarding session — bypassing intent filter, forwarding to Python",
				"session", s.id, "chat", chatID)
			m.tracker.TrackBusinessIntent(s.id, chatID)
		} else {
			// Use the StateStore cache where available; fall back to heuristic
			// classification from the message text.  Media messages with no text body
			// are treated as business by default (customers routinely send voice notes
			// and images on a business number).
			var msgIntent intent.Intent
			fromCache := false
			if cachedIntent, ok := m.intentStore.Get(chatID); ok {
				msgIntent = cachedIntent
				fromCache = true
			} else if body != "" {
				msgIntent = intent.Classify(body)
			} else {
				// No textual signal - fail-open as business (media message).
				msgIntent = intent.IntentBusiness
			}
			m.tracker.TrackIntentClassified(s.id, chatID, msgIntent.String(), fromCache)

			switch msgIntent {
			case intent.IntentBusiness:
				// Refresh cache so the TTL resets on continued business conversation.
				m.intentStore.Set(chatID, intent.IntentBusiness)
				m.tracker.TrackBusinessIntent(s.id, chatID)
				slog.Info("[bridge] intent: business - forwarding to AI",
					"session", s.id, "chat", chatID, "from_cache", fromCache)

			case intent.IntentPersonal:
				m.intentStore.Set(chatID, intent.IntentPersonal)
				m.tracker.TrackPersonalIntent(s.id, chatID)
				m.tracker.TrackAIReplySkipped(s.id, chatID, info.ID, "personal_intent")
				slog.Info("[bridge] intent: personal - skipping AI reply",
					"session", s.id, "chat", chatID, "msg_id", info.ID, "from_cache", fromCache)
				return

			default: // IntentUnclear - do not cache; re-evaluate on the next message.
				m.tracker.TrackUnclearIntent(s.id, chatID)
				m.tracker.TrackAIReplySkipped(s.id, chatID, info.ID, "unclear_intent")
				slog.Info("[bridge] intent: unclear - skipping AI reply",
					"session", s.id, "chat", chatID, "msg_id", info.ID)
				return
			}
		}

		payload := webhook.Event{
			Event:    "message",
			DeviceID: s.id,
			Phone:    ownPhone,
			Payload: webhook.MessagePayload{
				ChatID:      chatID,
				From:        from,
				SenderPN:    senderPN,
				PushName:    info.PushName,
				Body:        body,
				MessageID:   info.ID,
				Timestamp:   info.Timestamp.Unix(),
				IsFromMe:    info.IsFromMe,
				IsGroup:     info.IsGroup,
				MessageType: msgType,
				MediaURL:    mediaURL,
				MimeType:    mimeType,
			},
		}

		m.sender.Send(payload)
		m.tracker.TrackAIReplySent(s.id, chatID, info.ID, msgType)
	}()
}

func classifyMessage(s *session, m *Manager, evt *events.Message) (msgType, body, mediaURL, mimeType string) {
	msg := evt.Message

	// Text
	if text := msg.GetConversation(); text != "" {
		return "text", text, "", ""
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" {
		return "text", ext.GetText(), "", ""
	}

	// PTT (voice note)
	if audio := msg.GetAudioMessage(); audio != nil {
		isPTT := audio.GetPTT()
		mt := "audio"
		if isPTT {
			mt = "ptt"
		}
		url, mime := m.downloadMedia(s, audio, "ogg")
		return mt, "", url, mime
	}

	// Image
	if img := msg.GetImageMessage(); img != nil {
		caption := img.GetCaption()
		url, mime := m.downloadMedia(s, img, "jpg")
		return "image", caption, url, mime
	}

	// Video
	if vid := msg.GetVideoMessage(); vid != nil {
		caption := vid.GetCaption()
		url, mime := m.downloadMedia(s, vid, "mp4")
		return "video", caption, url, mime
	}

	// Document
	if doc := msg.GetDocumentMessage(); doc != nil {
		ext := extensionFromMIME(doc.GetMimetype())
		url, mime := m.downloadMedia(s, doc, ext)
		return "document", doc.GetFileName(), url, mime
	}

	// Sticker / reaction — log at debug so drops are visible
	if msg.GetStickerMessage() != nil {
		slog.Debug("ignoring sticker message", "session", s.id)
		return "", "", "", ""
	}
	if msg.GetReactionMessage() != nil {
		slog.Debug("ignoring reaction message", "session", s.id)
		return "", "", "", ""
	}

	// Location share
	if loc := msg.GetLocationMessage(); loc != nil {
		lat := loc.GetDegreesLatitude()
		lng := loc.GetDegreesLongitude()
		body := fmt.Sprintf("LAT:%.7f,LNG:%.7f", lat, lng)
		return "location", body, "", ""
	}

	return "", "", "", ""
}

// downloadMedia downloads, saves, and returns (mediaURL, mimeType).
func (m *Manager) downloadMedia(s *session, dl whatsmeow.DownloadableMessage, defaultExt string) (string, string) {
	data, err := s.client.Download(context.Background(), dl)
	if err != nil {
		slog.Error("media download failed", "session", s.id, "error", err)
		return "", ""
	}

	ext := defaultExt
	mime := ""

	// Try to get MIME type from the message itself.
	type mimeTyper interface{ GetMimetype() string }
	if mt, ok := dl.(mimeTyper); ok {
		mime = mt.GetMimetype()
		if e := extensionFromMIME(mime); e != "" {
			ext = e
		}
	}

	if err := os.MkdirAll(m.mediaDir, 0o755); err != nil {
		slog.Error("create media dir failed", "error", err)
		return "", ""
	}

	filename := uuid.New().String() + "." + ext
	path := filepath.Join(m.mediaDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("write media file failed", "error", err)
		return "", ""
	}

	url := m.publicURL + "/media/" + filename
	return url, mime
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func extensionFromMIME(mimeType string) string {
	// Strip parameters (e.g. "audio/ogg; codecs=opus" → "audio/ogg").
	mediaType, _, _ := mime.ParseMediaType(mimeType)
	switch mediaType {
	case "audio/ogg":
		return "ogg"
	case "audio/mpeg":
		return "mp3"
	case "audio/mp4", "audio/aac":
		return "m4a"
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "video/mp4":
		return "mp4"
	case "application/pdf":
		return "pdf"
	default:
		exts, _ := mime.ExtensionsByType(mimeType)
		if len(exts) > 0 {
			return strings.TrimPrefix(exts[0], ".")
		}
		return "bin"
	}
}

// ParsePhone accepts both raw digits and JID format and returns a types.JID.
func ParsePhone(phone string) (types.JID, error) {
	phone = strings.TrimSpace(phone)
	// Already a JID (contains '@')
	if strings.Contains(phone, "@") {
		return types.ParseJID(phone)
	}
	// Raw digits
	digits := digitsOnly(phone)
	if digits == "" {
		return types.JID{}, fmt.Errorf("empty phone number")
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

// BuildTextMessage wraps plain text in a waE2E.Message.
func BuildTextMessage(text string) *waE2E.Message {
	return &waE2E.Message{
		Conversation: proto.String(text),
	}
}
