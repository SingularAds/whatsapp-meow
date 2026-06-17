// Package intent now exposes a single decision: whether a sender's
// saved contact name marks them as a personal relationship of the
// business owner (mother, father, brother, etc.).
//
// Intent classification of message *content* has moved to the Python
// backend (services/intent_classifier.py), where an LLM with
// conversation history can reason about continuity ("12pm" after
// "what time?") that a keyword matcher cannot.
//
// The bridge's only gate is this contact-name check: if the owner
// saved the sender as e.g. "Mom" or "Bhai", we never forward — the
// AI must never reply to family. Everything else is forwarded and
// the backend decides.
package intent

import (
	"strings"
	"unicode"
)

// personalRelationshipTerms are case-insensitive whole-word labels the
// owner might have used when saving a personal contact in their phone
// address book. Matching is whole-word, so "Tom" never matches "mom".
//
// Covers English, Hindi (Roman script, common in India), Portuguese,
// and Spanish — the languages this product currently ships in.
var personalRelationshipTerms = map[string]struct{}{
	// ── English ──────────────────────────────────────────────────────
	"mom": {}, "mum": {}, "mother": {}, "ma": {}, "mama": {}, "mommy": {}, "mommie": {}, "mummy": {},
	"dad": {}, "daddy": {}, "father": {}, "papa": {}, "pop": {},
	"husband": {}, "hubby": {}, "wife": {},
	"brother": {}, "bro": {}, "sister": {}, "sis": {},
	"son": {}, "daughter": {}, "kid": {},
	"uncle": {}, "aunt": {}, "aunty": {}, "auntie": {},
	"grandma": {}, "grandpa": {}, "granny": {}, "granddad": {}, "grandmother": {}, "grandfather": {},
	"cousin": {}, "nephew": {}, "niece": {},
	"boyfriend": {}, "girlfriend": {}, "bf": {}, "gf": {},
	"fiance": {}, "fiancee": {},
	// ── Hindi (Roman script) ─────────────────────────────────────────
	"maa": {}, "maaji": {}, "bhai": {}, "bhaiya": {}, "bhaiyaa": {},
	"didi": {}, "behen": {}, "behan": {},
	"chacha": {}, "chachi": {}, "mami": {}, "mausi": {}, "mausa": {}, "tau": {}, "tai": {},
	"nani": {}, "nana": {}, "dadi": {}, "dada": {},
	"pati": {}, "patni": {}, "biwi": {},
	// ── Portuguese ───────────────────────────────────────────────────
	"mae": {}, "pai": {},
	"irmao": {}, "irma": {},
	"marido": {}, "esposa": {}, "esposo": {},
	"filho": {}, "filha": {},
	"tio": {}, "tia": {}, "primo": {}, "prima": {},
	"avo": {},
	"namorado": {}, "namorada": {}, "noivo": {}, "noiva": {},
	// ── Spanish ──────────────────────────────────────────────────────
	"madre": {}, "padre": {},
	"hermano": {}, "hermana": {},
	"hijo": {}, "hija": {},
	"abuelo": {}, "abuela": {},
	"sobrino": {}, "sobrina": {},
	"novio": {}, "novia": {},
}

// IsPersonalContactName reports whether name (typically the owner's
// FirstName/FullName label for the sender, NOT the sender's PushName)
// contains a whole word that marks the sender as a personal relation.
//
// Diacritics are stripped before matching so "Mãe" matches "mae".
// Empty names return false (an unsaved contact is not "personal" — it's
// just unknown, which the backend will classify).
func IsPersonalContactName(name string) bool {
	if name == "" {
		return false
	}
	for _, w := range tokenize(name) {
		if _, ok := personalRelationshipTerms[w]; ok {
			return true
		}
	}
	return false
}

// tokenize lowercases name, strips combining marks (so "Mãe" → "mae"),
// and splits on any non-letter rune. Returns the resulting word list.
func tokenize(name string) []string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r):
			b.WriteRune(foldDiacritic(r))
		case unicode.IsMark(r):
			// Skip combining marks so "ã" (a + combining tilde) folds to "a".
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// foldDiacritic maps a small set of precomposed Latin letters with
// diacritics to their plain ASCII counterparts. Covers the characters
// that appear in the languages listed in personalRelationshipTerms;
// anything else is returned unchanged.
func foldDiacritic(r rune) rune {
	switch r {
	case 'á', 'à', 'â', 'ã', 'ä', 'å':
		return 'a'
	case 'é', 'è', 'ê', 'ë':
		return 'e'
	case 'í', 'ì', 'î', 'ï':
		return 'i'
	case 'ó', 'ò', 'ô', 'õ', 'ö':
		return 'o'
	case 'ú', 'ù', 'û', 'ü':
		return 'u'
	case 'ñ':
		return 'n'
	case 'ç':
		return 'c'
	}
	return r
}
