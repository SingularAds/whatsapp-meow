package store

import (
	"context"
	"fmt"
	"os"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite" // register "sqlite" driver (CGo-free)
)

// NewContainer creates (or opens) the SQLite session container for the given session ID.
// The database file is stored at <dbDir>/<sessionID>.db.
func NewContainer(sessionID, dbDir string) (*sqlstore.Container, error) {
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create db dir %q: %w", dbDir, err)
	}

	dbPath := fmt.Sprintf("%s/%s.db", dbDir, sessionID)
	// _pragma=foreign_keys(1)   — required by whatsmeow schema
	// _pragma=journal_mode(WAL) — WAL allows concurrent readers + 1 writer;
	//                             eliminates SQLITE_BUSY on parallel whatsmeow writes
	// _pragma=busy_timeout(5000)— wait up to 5 s before returning SQLITE_BUSY
	// _pragma=synchronous(NORMAL)— safe with WAL, avoids excessive fsync overhead
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
		dbPath,
	)

	logger := waLog.Stdout("DB-"+sessionID, "WARN", true)
	container, err := sqlstore.New(context.Background(), "sqlite", dsn, logger)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store for session %q: %w", sessionID, err)
	}
	return container, nil
}
