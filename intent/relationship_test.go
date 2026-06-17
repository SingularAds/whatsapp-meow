package intent

import "testing"

func TestIsPersonalContactName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// ── Positive: English ────────────────────────────────────────────
		{"plain mom", "Mom", true},
		{"mom with surname", "Mom Sharma", true},
		{"my dad prefix", "My Dad", true},
		{"father full label", "Father", true},
		{"husband", "Husband ❤️", true},
		{"brother short bro", "Bro", true},
		{"sister sis", "Sis", true},
		{"grandma", "Grandma Joan", true},
		{"boyfriend abbreviation bf", "BF", true},
		// ── Positive: Hindi (Roman) ──────────────────────────────────────
		{"hindi bhai", "Rohan Bhai", true},
		{"hindi didi", "Didi", true},
		{"hindi maa", "Maa", true},
		// ── Positive: Portuguese (with diacritics) ───────────────────────
		{"portuguese mae with tilde", "Mãe", true},
		{"portuguese irmao with tilde", "Irmão João", true},
		{"portuguese pai", "Pai", true},
		// ── Positive: Spanish ────────────────────────────────────────────
		{"spanish madre", "Madre", true},
		{"spanish hermano", "Hermano Luis", true},
		// ── Negative: false-positive guards (whole-word matching) ────────
		{"empty string", "", false},
		{"customer name Tom", "Tom", false},              // not "mom"
		{"customer name Sister St Pharmacy", "Brodie", false},
		{"customer name Dadson Inc", "Dadson", false},    // not "dad"
		{"customer name Brody Lee", "Brody Lee", false},  // not "bro"
		{"name with numbers", "Customer #42", false},
		{"random business name", "Joe Pizza Co", false},
		{"name with punctuation only", "...", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPersonalContactName(tc.in)
			if got != tc.want {
				t.Errorf("IsPersonalContactName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
