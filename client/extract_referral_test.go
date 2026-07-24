package client

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// adMessage builds an ExtendedTextMessage carrying a Click-to-WhatsApp
// ExternalAdReply — the shape WhatsApp delivers for the prospect's first
// message after tapping a Meta/Instagram "Send message" ad.
func adMessage(ad *waE2E.ContextInfo_ExternalAdReplyInfo) *waE2E.Message {
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("Olá! Posso ter mais informações sobre isso?"),
			ContextInfo: &waE2E.ContextInfo{ExternalAdReply: ad},
		},
	}
}

// TestExtractReferral_CTWA verifies the CTWA ad referral is mapped onto the
// webhook ReferralInfo with the exact field names the backend consumes.
func TestExtractReferral_CTWA(t *testing.T) {
	msg := adMessage(&waE2E.ContextInfo_ExternalAdReplyInfo{
		SourceID:   proto.String("120210000000000000"),
		SourceURL:  proto.String("https://instagram.com/recepte.co"),
		SourceType: proto.String("ad"),
		CtwaClid:   proto.String("AbCdEf123"),
		Title:      proto.String("9 agendamentos enquanto você dormia"),
		Body:       proto.String("Automação WhatsApp + recepte = marketing"),
	})

	ref := extractReferral(msg)
	if ref == nil {
		t.Fatal("expected a referral, got nil")
	}
	if ref.SourceID != "120210000000000000" {
		t.Errorf("SourceID = %q", ref.SourceID)
	}
	if ref.SourceURL != "https://instagram.com/recepte.co" {
		t.Errorf("SourceURL = %q", ref.SourceURL)
	}
	if ref.SourceType != "ad" {
		t.Errorf("SourceType = %q", ref.SourceType)
	}
	if ref.CtwaClid != "AbCdEf123" {
		t.Errorf("CtwaClid = %q", ref.CtwaClid)
	}
	if ref.Title == "" || ref.Body == "" {
		t.Errorf("Title/Body should be populated: %q / %q", ref.Title, ref.Body)
	}
}

// TestExtractReferral_ImageAd covers image-format ads (ExternalAdReply on an
// ImageMessage's ContextInfo instead of ExtendedTextMessage).
func TestExtractReferral_ImageAd(t *testing.T) {
	msg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			ContextInfo: &waE2E.ContextInfo{
				ExternalAdReply: &waE2E.ContextInfo_ExternalAdReplyInfo{
					SourceID:  proto.String("999"),
					SourceURL: proto.String("https://fb.me/xyz"),
				},
			},
		},
	}
	ref := extractReferral(msg)
	if ref == nil || ref.SourceID != "999" {
		t.Fatalf("expected image-ad referral with SourceID=999, got %+v", ref)
	}
}

// TestExtractReferral_NonAd verifies that ordinary (organic) messages, and a
// degenerate ExternalAdReply with no identifying signal, produce NO referral —
// so nothing ever manufactures a bogus "ad" attribution.
func TestExtractReferral_NonAd(t *testing.T) {
	cases := []struct {
		name string
		msg  *waE2E.Message
	}{
		{"plain conversation", &waE2E.Message{Conversation: proto.String("Hi, I want to sign up")}},
		{"extended text, no context", adMessage(nil)},
		{"empty ad reply", adMessage(&waE2E.ContextInfo_ExternalAdReplyInfo{
			Title: proto.String("headline only, no ids"),
		})},
		{"nil message", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ref := extractReferral(tc.msg); ref != nil {
				t.Errorf("expected nil referral, got %+v", ref)
			}
		})
	}
}
