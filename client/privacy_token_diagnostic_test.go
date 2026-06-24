package client

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// All the our_jid / their_jid values in these tests are copied verbatim from
// the actual SQLite dump taken from the running bridge — see the smba.db
// inspection results. The point of using real shapes is so the classifier
// stays correct against (a) the ":NN" device-session-id suffix WhatsApp
// assigns at pair time and (b) the @lid vs @s.whatsapp.net split that
// privacy-mode contacts cause.

// Verbatim from the production SQLite dump:
//
//	whatsmeow_privacy_tokens.our_jid = 918968012547:49@s.whatsapp.net  (pre-repair session)
//	whatsmeow_app_state_mutation_macs.jid = 918968012547:51@s.whatsapp.net  (post-repair session)
//
//	their_jid values (all @lid because senders are privacy-mode):
//	  241347130871842@lid  → 918294746282  (Abhishek)
//	  165592464113781@lid  → 919068319516  (Ankit iCY)
//	  269693898207295@lid  → (unknown)
const (
	preRepairOurJID  = "918968012547:49@s.whatsapp.net"
	postRepairOurJID = "918968012547:51@s.whatsapp.net"

	abhishekLID   = "241347130871842@lid"
	ankitIcyLID   = "165592464113781@lid"
	unknownLID3rd = "269693898207295@lid"
)

func TestClassifyTokenCacheState(t *testing.T) {
	cases := []struct {
		name           string
		currentOurJID  string
		tokensByOurJID map[string]int
		want           TokenCacheState
	}{
		{
			name:           "empty currentOurJID returns Unknown",
			currentOurJID:  "",
			tokensByOurJID: map[string]int{preRepairOurJID: 3},
			want:           TokenCacheUnknown,
		},
		{
			name:           "no tokens at all is Empty (fresh pair, no traffic yet)",
			currentOurJID:  postRepairOurJID,
			tokensByOurJID: map[string]int{},
			want:           TokenCacheEmpty,
		},
		{
			name:           "all 3 production tokens stranded under :49 while current is :51 is Stranded",
			currentOurJID:  postRepairOurJID,
			tokensByOurJID: map[string]int{preRepairOurJID: 3},
			want:           TokenCacheStranded,
		},
		{
			name:           "current session has its own token plus stranded ones is Healthy",
			currentOurJID:  postRepairOurJID,
			tokensByOurJID: map[string]int{preRepairOurJID: 2, postRepairOurJID: 1},
			want:           TokenCacheHealthy,
		},
		{
			name:           "current session has tokens, none stranded, is Healthy",
			currentOurJID:  postRepairOurJID,
			tokensByOurJID: map[string]int{postRepairOurJID: 5},
			want:           TokenCacheHealthy,
		},
		{
			name:           "multiple stranded session ids reproduce the bug after multiple re-pairs",
			currentOurJID:  postRepairOurJID,
			tokensByOurJID: map[string]int{preRepairOurJID: 3, "918968012547:50@s.whatsapp.net": 2},
			want:           TokenCacheStranded,
		},
		{
			name:           "single visible token among many stranded is still Healthy (one good recipient is enough to not warn)",
			currentOurJID:  postRepairOurJID,
			tokensByOurJID: map[string]int{preRepairOurJID: 10, postRepairOurJID: 1},
			want:           TokenCacheHealthy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTokenCacheState(tc.currentOurJID, tc.tokensByOurJID)
			if got != tc.want {
				t.Fatalf("classifyTokenCacheState(%q, %v) = %v, want %v",
					tc.currentOurJID, tc.tokensByOurJID, got, tc.want)
			}
		})
	}
}

func TestSessionDBPath(t *testing.T) {
	// Mirror what Manager constructs at client/whatsapp.go:419
	got := sessionDBPath("./data", "smba")
	want := filepath.Join("./data", "smba.db")
	if got != want {
		t.Fatalf("sessionDBPath = %q, want %q", got, want)
	}
}

// TestReadPrivacyTokenHistogram_RealisticPayload constructs a SQLite file
// with the same schema and three rows whatsmeow produces, mirroring the
// actual production state where all tokens are under the pre-repair our_jid.
// Verifies that the read path returns a histogram suitable for the
// classifier and that classifying it yields Stranded — which is the exact
// state we hit on the live bridge after re-pairing.
func TestReadPrivacyTokenHistogram_RealisticPayload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "smba.db")

	// Build a fixture DB with the real schema (subset of columns whatsmeow
	// uses) and the three rows we saw in the live store.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE whatsmeow_privacy_tokens (
			our_jid          TEXT NOT NULL,
			their_jid        TEXT NOT NULL,
			token            BLOB NOT NULL,
			timestamp        INTEGER NOT NULL,
			sender_timestamp INTEGER,
			PRIMARY KEY (our_jid, their_jid)
		)
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	insert := `INSERT INTO whatsmeow_privacy_tokens (our_jid, their_jid, token, timestamp, sender_timestamp) VALUES (?, ?, ?, ?, ?)`
	// Three rows matching the live store dump exactly.
	rows := []struct {
		ourJID   string
		theirJID string
		ts       int64
		senderTS int64
	}{
		{preRepairOurJID, abhishekLID, 1782106465, 1782106478},
		{preRepairOurJID, ankitIcyLID, 1782110676, 1782110689},
		{preRepairOurJID, unknownLID3rd, 1782111341, 1782111359},
	}
	for _, r := range rows {
		if _, err := db.Exec(insert, r.ourJID, r.theirJID, []byte("11_byte_tok"), r.ts, r.senderTS); err != nil {
			t.Fatalf("insert %+v: %v", r, err)
		}
	}
	db.Close()

	hist, err := readPrivacyTokenHistogram(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("readPrivacyTokenHistogram: %v", err)
	}
	if hist[preRepairOurJID] != 3 {
		t.Fatalf("expected 3 tokens under %q, got %v (full hist: %v)", preRepairOurJID, hist[preRepairOurJID], hist)
	}
	if len(hist) != 1 {
		t.Fatalf("expected single our_jid bucket, got %v", hist)
	}

	// End-to-end: feeding the histogram to the classifier with the
	// post-repair our_jid must reproduce the production failure mode.
	if state := classifyTokenCacheState(postRepairOurJID, hist); state != TokenCacheStranded {
		t.Fatalf("end-to-end: expected TokenCacheStranded for post-repair session against pre-repair tokens, got %v", state)
	}
}

func TestReadPrivacyTokenHistogram_MissingTableIsHandled(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Touch the DB with no tables, then close.
	if _, err := db.Exec(`CREATE TABLE placeholder (x INTEGER)`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	db.Close()

	_, err = readPrivacyTokenHistogram(context.Background(), dbPath)
	if err == nil {
		t.Fatal("expected error when whatsmeow_privacy_tokens table is missing; got nil")
	}
}
