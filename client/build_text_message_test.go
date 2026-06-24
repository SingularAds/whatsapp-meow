package client

import (
	"strings"
	"testing"
)

// TestBuildTextMessage_PlainTextUsesConversation verifies that plain text (the
// vast majority of outbound messages — onboarding greetings, AI replies, etc.)
// is sent via the Conversation field. This is the low-scrutiny path on
// WhatsApp's anti-abuse pipeline and avoids the 463 reach-out-time-lock that
// rejected first-contact onboarding sends when ExtendedTextMessage was used
// for everything.
func TestBuildTextMessage_PlainTextUsesConversation(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"onboarding greeting", "Hi Amit! I'm Sofia 👋 I'm about to become your business's new best friend. Got a website, Google Maps, or Instagram? Drop it here and I'll set everything up for you ✨ No link? Just tell me your business name 😊"},
		{"plain ack", "Got it, thanks!"},
		{"emoji only", "👍✨🚀"},
		{"single word", "ok"},
		{"empty", ""},
		{"english sentence", "Your booking is confirmed for tomorrow at 3pm."},
		{"portuguese sentence", "A tua marcação está confirmada para amanhã às 15h."},
		{"numbers and punctuation", "Total: 49.99 EUR. Confirmed?"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := BuildTextMessage(tc.text)
			if msg == nil {
				t.Fatal("BuildTextMessage returned nil")
			}
			if msg.Conversation == nil {
				t.Fatalf("expected Conversation field set for plain text %q, got nil (ExtendedTextMessage=%v)", tc.text, msg.ExtendedTextMessage != nil)
			}
			if msg.ExtendedTextMessage != nil {
				t.Fatalf("plain text %q must NOT use ExtendedTextMessage (triggers WhatsApp error 463 on cold contacts)", tc.text)
			}
			if *msg.Conversation != tc.text {
				t.Fatalf("Conversation field mismatch: got %q want %q", *msg.Conversation, tc.text)
			}
		})
	}
}

// TestBuildTextMessage_FormattedTextUsesExtended verifies that text containing
// WhatsApp markdown delimiters is routed through ExtendedTextMessage so the
// recipient's client renders bold/italic/code/strike correctly.
func TestBuildTextMessage_FormattedTextUsesExtended(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"bold", "Hello *world*"},
		{"italic", "_emphasised_"},
		{"monospace", "Run `npm install`"},
		{"strikethrough", "~old price~ now $19"},
		{"code block", "```\ngit status\n```"},
		{"mixed", "*Bold* and _italic_ together"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := BuildTextMessage(tc.text)
			if msg == nil {
				t.Fatal("BuildTextMessage returned nil")
			}
			if msg.ExtendedTextMessage == nil {
				t.Fatalf("expected ExtendedTextMessage for formatted text %q, got Conversation=%v", tc.text, msg.Conversation != nil)
			}
			if msg.Conversation != nil {
				t.Fatalf("formatted text %q must not also set Conversation", tc.text)
			}
			if msg.ExtendedTextMessage.Text == nil || *msg.ExtendedTextMessage.Text != tc.text {
				t.Fatalf("ExtendedTextMessage.Text mismatch")
			}
		})
	}
}

// TestHasWhatsAppFormatting documents the exact set of characters that trigger
// the ExtendedTextMessage path. If WhatsApp adds new markdown delimiters this
// test should be updated to keep the behavior explicit.
func TestHasWhatsAppFormatting(t *testing.T) {
	yes := []string{"*x*", "_x_", "`x`", "~x~", "mix *of* _things_"}
	no := []string{"", "plain text", "Hi Amit!", "👋✨", "https://example.com", "a.b@c.com"}

	for _, s := range yes {
		if !hasWhatsAppFormatting(s) {
			t.Errorf("expected formatting in %q", s)
		}
	}
	for _, s := range no {
		if hasWhatsAppFormatting(s) {
			t.Errorf("did NOT expect formatting in %q", s)
		}
	}

	// Sanity: the function only looks at the delimiter set, nothing else.
	if hasWhatsAppFormatting("") {
		t.Error("empty string should not have formatting")
	}
	if !strings.ContainsAny("*_`~", "*") {
		t.Fatal("test invariant broken")
	}
}
