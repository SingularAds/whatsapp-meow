// Package intent provides heuristic-based message intent classification.
// It classifies incoming WhatsApp messages as business, personal, or unclear
// so the bridge can decide whether to forward a message to AI automation.
package intent

import (
	"strings"
	"unicode"
)

// Intent represents the classified purpose of an incoming message.
type Intent int

const (
	// IntentBusiness means the message is a customer/business enquiry.
	IntentBusiness Intent = iota
	// IntentPersonal means the message is a clearly personal conversation.
	IntentPersonal
	// IntentUnclear means the intent cannot be determined from this message alone.
	IntentUnclear
)

// String returns a human-readable label for the intent.
func (i Intent) String() string {
	switch i {
	case IntentBusiness:
		return "business"
	case IntentPersonal:
		return "personal"
	default:
		return "unclear"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Keyword lists
// ─────────────────────────────────────────────────────────────────────────────

// strongPersonalKeywords are highly reliable signals of personal conversation.
// Even one match shifts the classification to PERSONAL.
var strongPersonalKeywords = []string{
	// Relationship terms
	"my wife", "my husband", "my boyfriend", "my girlfriend",
	"my mom", "my mum", "my dad", "my father", "my mother",
	"my brother", "my sister", "my son", "my daughter",
	"my uncle", "my aunty", "my aunt", "my cousin",
	"my grandma", "my grandpa", "my fiancé", "my fiance",
	// Personal affection phrases
	"i love you", "love you", "i miss you", "miss you",
	"thinking of you", "thinking about you",
	// Social plans (clearly personal)
	"coming to the party", "coming over tonight", "see you tonight",
	"dinner tonight", "lunch today with", "drinks tonight",
	"sleepover", "watching netflix", "going clubbing",
	// Complaint/personal distress (not business)
	"are you okay", "i'm worried about you", "worried about you",
	"take care of yourself", "get well soon my",
}

// weakPersonalKeywords add points toward PERSONAL but don't decide alone.
var weakPersonalKeywords = []string{
	"dude", "bro ", " bro,", " bro.", "bestie", "buddy",
	"lmao", "rofl", "lolol", "hahaha", "hehehehe",
	"wanna hang", "wanna come", "wanna meet",
	"you free?", "you free tonight", "free tonight",
	"coming home", "on my way home",
}

// businessKeywords are reliable signals of a customer/service enquiry.
var businessKeywords = []string{
	// Booking / appointment
	"book", "booking", "appointment", "reserve", "reservation",
	"schedule", "reschedule", "slot", "available slot",
	// Pricing / cost
	"price", "pricing", "how much", "cost", "costs", "rate ", "rates",
	"fee", "fees", "charge", "charges", "quote", "estimate",
	"package", "packages", "offer", "offers", "discount",
	// Service inquiry
	"service", "services", "provide", "do you do", "do you offer",
	"what do you", "menu", "catalog", "catalogue",
	// Availability
	"available", "availability", "open", "opening hours", "working hours",
	"timing", "timings", "business hours", "when do you",
	// Location / contact
	"address", "location", "where are you", "how to get", "directions",
	// Order / status
	"order", "status", "delivery", "pickup", "pick up", "collect",
	"track", "tracking", "shipment",
	// Payment
	"payment", "pay", "invoice", "receipt",
	// General inquiry
	"inquiry", "enquiry", "information", "info", "details",
	"can i get", "i need", "i want to", "i would like",
	"cancel", "cancellation",
	// Product questions
	"product", "products", "item", "items", "stock", "in stock",
	// Complaint/feedback (business context)
	"complaint", "not working", "broken", "defective", "return", "refund",
	// Job / work inquiry
	"vacancy", "vacancies", "hiring", "job opening", "apply for",
}

// greetingKeywords are ambiguous but typically signal a customer opening.
// On a registered business number any greeting from an unknown party is
// treated as a potential customer enquiry (fail-open for business).
var greetingKeywords = []string{
	"hello", "hi ", "hi!", "hi,", "^hi$",
	"hey", "hola", "good morning", "good afternoon", "good evening",
	"greetings", "namaste", "salaam", "assalamualaikum",
	"howdy",
}

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

// Classify returns the most likely intent of a message body.
// Strategy (in order of precedence):
//  1. Strong personal marker present  → PERSONAL
//  2. Business keyword present        → BUSINESS
//  3. Weak personal markers dominate  → PERSONAL
//  4. Pure greeting / very short msg  → BUSINESS (new customer opening)
//  5. Fallback                        → UNCLEAR
func Classify(text string) Intent {
	lower := normalizeForSearch(text)

	// ── Strong personal check ────────────────────────────────────────────────
	for _, kw := range strongPersonalKeywords {
		if strings.Contains(lower, kw) {
			return IntentPersonal
		}
	}

	// ── Business check ───────────────────────────────────────────────────────
	businessScore := 0
	for _, kw := range businessKeywords {
		if strings.Contains(lower, kw) {
			businessScore++
		}
	}

	if businessScore > 0 {
		return IntentBusiness
	}

	// ── Weak personal check ──────────────────────────────────────────────────
	weakPersonalScore := 0
	for _, kw := range weakPersonalKeywords {
		if strings.Contains(lower, kw) {
			weakPersonalScore++
		}
	}

	if weakPersonalScore >= 2 {
		return IntentPersonal
	}

	// ── Pure greeting / very short message ───────────────────────────────────
	// On a business number, an opening message of "Hi" or "Hello" is almost
	// certainly a customer starting a conversation — treat as business.
	if isGreetingOrShort(lower) {
		return IntentBusiness
	}

	return IntentUnclear
}

// ClassifyWithHistory determines intent using both the current message and a
// slice of previous messages from the same conversation (oldest first).
// If any previous message was already classified, that classification wins
// (handled by StateStore above the classifier — this function re-derives it
// from raw history for callers that have the raw messages available).
func ClassifyWithHistory(current string, history []string) Intent {
	// First try the current message.
	result := Classify(current)
	if result != IntentUnclear {
		return result
	}

	// Current message is unclear — scan history for clearer signal.
	for i := len(history) - 1; i >= 0; i-- {
		h := Classify(history[i])
		if h != IntentUnclear {
			return h
		}
	}

	return IntentUnclear
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// normalizeForSearch lower-cases the text and collapses unicode whitespace so
// all keyword comparisons work uniformly.
func normalizeForSearch(text string) string {
	// Map to lower-case and collapse internal whitespace to single spaces.
	var b strings.Builder
	b.Grow(len(text))
	prevSpace := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
			}
			prevSpace = true
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	result := strings.TrimSpace(b.String())
	// Pad with spaces so word-boundary prefix/suffix checks on keywords work.
	return " " + result + " "
}

// isGreetingOrShort returns true if the normalised text contains only a
// greeting word (or is very short — ≤ 3 words), which on a business line is
// treated as a customer conversation opener.
func isGreetingOrShort(lower string) bool {
	trimmed := strings.TrimSpace(lower)
	for _, g := range greetingKeywords {
		if strings.Contains(trimmed, g) {
			return true
		}
	}
	// Very short messages on a business line are likely customer openers.
	words := strings.Fields(trimmed)
	return len(words) <= 3
}
