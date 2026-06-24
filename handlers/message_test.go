package handlers

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsReachoutTimelocked verifies the error classifier that decides whether
// a SendMessage failure should trigger the 463 retry path. The literal
// substring "server returned error 463" is what whatsmeow's send.go embeds
// in the wrapped error when WhatsApp's anti-abuse layer rejects the send.
func TestIsReachoutTimelocked(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"random 500", errors.New("internal server error"), false},
		{"wrong code 429", errors.New("server returned error 429"), false},
		{"wrong code 405", errors.New("server returned error 405"), false},
		{"plain 463", errors.New("server returned error 463"), true},
		{"wrapped 463", fmt.Errorf("send failed: %w", errors.New("server returned error 463")), true},
		{"trailing content", errors.New("server returned error 463 (NackCallerReachoutTimelocked)"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReachoutTimelocked(tc.err); got != tc.want {
				t.Fatalf("isReachoutTimelocked(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetryDelaysAreSane guards two real-world failure modes:
//
//  1. Upper bound — the Python backend's httpx client uses a 30s timeout when
//     calling POST /send/message (see app/services/whatsmeow_client.py
//     _client(timeout=30.0)). Our cumulative retry wait + per-attempt send
//     latency MUST stay well under that, otherwise the backend cancels mid-
//     retry, the bridge sees "context canceled", the 429 mapping never fires,
//     and the backend swallows the failure as a generic exception with no
//     state marker. We cap sum-of-delays at 20s to leave 10s for the actual
//     SendMessage calls.
//
//  2. Lower bound — whatsmeow's send.go schedules an async
//     issuePrivacyTokenAndSave AFTER a failed send. That IQ + the server's
//     privacy_token notification need 1–5s to complete. A first retry under
//     5s is essentially useless: we'd retry before the token could possibly
//     have been issued.
func TestRetryDelaysAreSane(t *testing.T) {
	if len(retryDelays) == 0 {
		t.Fatal("retryDelays must have at least one entry; otherwise the helper degenerates to a single attempt")
	}
	var total int64
	for i, d := range retryDelays {
		if d <= 0 {
			t.Fatalf("retryDelays[%d] must be positive; got %v", i, d)
		}
		total += int64(d.Seconds())
	}
	const maxBudgetSeconds = 20
	if total > maxBudgetSeconds {
		t.Fatalf("sum of retryDelays = %ds exceeds %ds budget — backend httpx timeout (30s) will cancel mid-retry, defeating the typed 429 mapping", total, maxBudgetSeconds)
	}
	if retryDelays[0] < 5_000_000_000 { // 5 seconds in ns
		t.Fatalf("first retry delay too short (%v) — whatsmeow's async privacy-token issuance won't complete in time", retryDelays[0])
	}
}
