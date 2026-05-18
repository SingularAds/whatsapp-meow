package intent

import (
	"sync"
	"time"
)

const (
	// defaultTTL is how long a conversation classification is cached before it
	// is re-evaluated. Set to 24 h so a customer who returns the next day gets
	// a fresh classification rather than re-using yesterday's cached result.
	defaultTTL = 24 * time.Hour

	// cleanupInterval is how often the background goroutine sweeps expired
	// entries from the map. Not performance-critical — once per hour is fine.
	cleanupInterval = 1 * time.Hour
)

// entry holds a single cached classification for a chat thread.
type entry struct {
	intent    Intent
	createdAt time.Time
}

// StateStore keeps lightweight conversation-level intent state so the bridge
// avoids re-classifying every message in an already-known chat.
//
// Thread-safe; designed to be created once and shared across goroutines.
type StateStore struct {
	mu      sync.RWMutex
	entries map[string]*entry
	ttl     time.Duration
	stopCh  chan struct{}
}

// NewStateStore returns a new StateStore with the default 24-hour TTL and
// starts a background goroutine that periodically removes expired entries.
func NewStateStore() *StateStore {
	ss := &StateStore{
		entries: make(map[string]*entry),
		ttl:     defaultTTL,
		stopCh:  make(chan struct{}),
	}
	go ss.cleanupLoop()
	return ss
}

// Get looks up the cached intent for chatID.
// Returns (intent, true) when a valid (non-expired) entry exists.
// Returns (IntentUnclear, false) when there is no entry or the entry has expired.
func (ss *StateStore) Get(chatID string) (Intent, bool) {
	ss.mu.RLock()
	e, ok := ss.entries[chatID]
	ss.mu.RUnlock()

	if !ok {
		return IntentUnclear, false
	}
	if time.Since(e.createdAt) > ss.ttl {
		// Expired — delete lazily on the next write lock.
		ss.mu.Lock()
		delete(ss.entries, chatID)
		ss.mu.Unlock()
		return IntentUnclear, false
	}
	return e.intent, true
}

// Set stores (or overwrites) the intent classification for chatID.
// A new creation timestamp is recorded so the TTL resets on re-classification.
func (ss *StateStore) Set(chatID string, i Intent) {
	ss.mu.Lock()
	ss.entries[chatID] = &entry{intent: i, createdAt: time.Now()}
	ss.mu.Unlock()
}

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (ss *StateStore) Close() {
	select {
	case <-ss.stopCh:
		// Already closed.
	default:
		close(ss.stopCh)
	}
}

// cleanupLoop runs in a background goroutine and removes expired entries every
// cleanupInterval. It exits when the store is closed.
func (ss *StateStore) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ss.stopCh:
			return
		case <-ticker.C:
			ss.sweep()
		}
	}
}

func (ss *StateStore) sweep() {
	now := time.Now()
	ss.mu.Lock()
	for id, e := range ss.entries {
		if now.Sub(e.createdAt) > ss.ttl {
			delete(ss.entries, id)
		}
	}
	ss.mu.Unlock()
}
