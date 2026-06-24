package client

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// TokenCacheState classifies the visibility of the privacy-token cache for
// the bridge's current device session.
//
// Background: whatsmeow stores privacy tokens (the per-recipient credentials
// that authorize outbound sends from a companion device) keyed on the FULL
// device JID, e.g. "918968012547:51@s.whatsapp.net". The ":51" is the
// multi-device session ID assigned by WhatsApp's server at pair time. When
// the device is re-paired, that suffix rotates (e.g. :49 → :51), and
// whatsmeow's getPrivacyToken SQL filters by the full our_jid. Result: every
// token issued before the re-pair becomes invisible to the new session,
// even though the rows still physically exist on disk. The bridge then
// behaves as if every recipient is cold and returns 463 for every first
// send to people that previously worked.
//
// The TokenCacheStranded value is the unambiguous signature of that
// failure mode and the only one we log loudly.
type TokenCacheState int

const (
	// TokenCacheUnknown means the diagnostic could not run — usually a
	// transient DB-access error at startup. Treated as benign.
	TokenCacheUnknown TokenCacheState = iota

	// TokenCacheEmpty means the cache contains no privacy tokens at all.
	// Normal for a freshly-paired device that has not yet built up trust;
	// not necessarily a problem — tokens accumulate as recipients message
	// the device.
	TokenCacheEmpty

	// TokenCacheHealthy means at least one token is stored under the
	// session's current our_jid, so the bridge can authenticate sends to
	// those specific recipients.
	TokenCacheHealthy

	// TokenCacheStranded is the failure mode this diagnostic exists for:
	// tokens exist on disk but ALL of them belong to a previous device
	// session ID. Cold sends will fail with 463 until either (a) recipients
	// re-trigger token issuance for the new session, typically by the
	// primary phone manually messaging them, or (b) the recipient population
	// turns over completely.
	TokenCacheStranded
)

// classifyTokenCacheState reports which TokenCacheState the cache is in given
// the current session's our_jid and a histogram of token counts by stored
// our_jid. The function is pure so the failure-mode classification can be
// unit-tested against the exact our_jid shapes seen in the SQLite store.
func classifyTokenCacheState(currentOurJID string, tokensByOurJID map[string]int) TokenCacheState {
	if currentOurJID == "" {
		return TokenCacheUnknown
	}
	if len(tokensByOurJID) == 0 {
		return TokenCacheEmpty
	}
	visible := tokensByOurJID[currentOurJID]
	if visible > 0 {
		return TokenCacheHealthy
	}
	// No tokens visible to the current session, but at least one token row
	// exists under a different our_jid → re-pair-invalidation.
	return TokenCacheStranded
}

// sessionDBPath returns the absolute path to the SQLite file the bridge uses
// for a given session.  Mirrors the path used by Manager (dbDir + "<id>.db").
func sessionDBPath(dbDir, sessionID string) string {
	return filepath.Join(dbDir, sessionID+".db")
}

// readPrivacyTokenHistogram opens the session's SQLite file read-only and
// returns a histogram of token counts grouped by our_jid. The connection is
// opened with ?mode=ro and a short busy timeout so it never blocks the live
// bridge process that holds the write handle.
func readPrivacyTokenHistogram(ctx context.Context, dbPath string) (map[string]int, error) {
	// Read-only handle: no journal_mode pragma — setting one requires writing
	// the journal header, which fails with "attempt to write a readonly
	// database (8)". SQLite will read from an existing WAL file on its own
	// when one is present, which is exactly what we want when the bridge has
	// the primary write handle open.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(2000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ro: %w", err)
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	rows, err := db.QueryContext(qCtx,
		`SELECT our_jid, COUNT(*) FROM whatsmeow_privacy_tokens GROUP BY our_jid`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	hist := map[string]int{}
	for rows.Next() {
		var ourJID string
		var n int
		if err := rows.Scan(&ourJID, &n); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		hist[ourJID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return hist, nil
}

// logTokenCacheHealth runs the diagnostic once for a connected session. Pure
// observability — no DB writes, no behavior change. Designed to be called
// from the events.Connected handler in a goroutine so a slow SQLite query
// can never delay the WhatsApp event loop. Logs at most one line, at WARN
// level for the stranded case and INFO for the others.
//
// dbPath is the path to the same SQLite file whatsmeow has open; we open a
// separate read-only handle via ?mode=ro so we cannot interfere with the
// live connection.  currentOurJID is the session's full device JID,
// typically obtained from cli.Store.ID.String().
func logTokenCacheHealth(ctx context.Context, sessionID, dbPath, currentOurJID string) {
	if currentOurJID == "" {
		// Can happen briefly before Store.ID is populated; skip silently.
		return
	}
	hist, err := readPrivacyTokenHistogram(ctx, dbPath)
	if err != nil {
		slog.Debug("[bridge] privacy-token diagnostic skipped (DB read failed)",
			"session", sessionID, "error", err)
		return
	}
	state := classifyTokenCacheState(currentOurJID, hist)
	total := 0
	otherJIDs := make([]string, 0, len(hist))
	for j, n := range hist {
		total += n
		if j != currentOurJID && n > 0 {
			otherJIDs = append(otherJIDs, j)
		}
	}

	switch state {
	case TokenCacheEmpty:
		slog.Info("[bridge] privacy-token cache is empty — first cold sends will fail with 463 until tokens are issued",
			"session", sessionID, "our_jid", currentOurJID)
	case TokenCacheHealthy:
		slog.Info("[bridge] privacy-token cache healthy",
			"session", sessionID,
			"our_jid", currentOurJID,
			"visible_tokens", hist[currentOurJID],
			"total_tokens_in_db", total)
	case TokenCacheStranded:
		// The actionable warning: re-pair has invalidated the cache.
		slog.Warn("[bridge] privacy tokens are STRANDED under previous device session(s) — every cold send will get 463 until new tokens are issued for the current session. This is the expected post-re-pair state.",
			"session", sessionID,
			"current_our_jid", currentOurJID,
			"stranded_our_jids", otherJIDs,
			"total_stranded_tokens", total,
			"action", "do not re-pair the device unless absolutely necessary; for each cold recipient, send one message from the linked phone (primary device) before the bridge replies")
	}
}
