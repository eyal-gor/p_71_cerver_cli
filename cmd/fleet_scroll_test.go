package cmd

import "testing"

// Alternate scroll mode turns a wheel notch into a BARE arrow, so bare arrows
// have to mean "scroll" and prompt history has to live on a modified arrow.
// splitModArrows is the seam: it lifts the modified ones out before the plain
// arrow count runs, so their trailing A/B is never counted as a scroll.
func TestSplitModArrows(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantRest string
		wantKeys []fleetKey
	}{
		{"bare up is left alone", "\x1b[A", "\x1b[A", nil},
		{"bare down is left alone", "\x1b[B", "\x1b[B", nil},
		{"ctrl+up", "\x1b[1;5A", "", []fleetKey{keyHistPrev}},
		{"ctrl+down", "\x1b[1;5B", "", []fleetKey{keyHistNext}},
		{"alt+up counts too", "\x1b[1;3A", "", []fleetKey{keyHistPrev}},
		{"mixed with a wheel burst", "\x1b[1;5A\x1b[A\x1b[A", "\x1b[A\x1b[A", []fleetKey{keyHistPrev}},
		{"two in one read", "\x1b[1;5A\x1b[1;5B", "", []fleetKey{keyHistPrev, keyHistNext}},
		{"right arrow untouched", "\x1b[1;5C", "\x1b[1;5C", nil},
		{"plain text", "hello", "hello", nil},
	}
	for _, c := range cases {
		rest, keys := splitModArrows(c.in)
		if rest != c.wantRest {
			t.Errorf("%s: rest = %q, want %q", c.name, rest, c.wantRest)
		}
		if len(keys) != len(c.wantKeys) {
			t.Errorf("%s: got %d keys, want %d", c.name, len(keys), len(c.wantKeys))
			continue
		}
		for i := range keys {
			if keys[i] != c.wantKeys[i] {
				t.Errorf("%s: key %d = %v, want %v", c.name, i, keys[i], c.wantKeys[i])
			}
		}
	}
}

// The wheel arriving one notch at a time is the whole bug: a single bare arrow
// is indistinguishable from a keypress, so nothing downstream may rely on the
// burst heuristic to tell them apart in the session view.
func TestSingleWheelNotchLeavesNoHistoryKey(t *testing.T) {
	rest, keys := splitModArrows("\x1b[A")
	if len(keys) != 0 {
		t.Errorf("a lone bare arrow must not become history: %v", keys)
	}
	if rest != "\x1b[A" {
		t.Errorf("rest = %q, want the arrow back untouched", rest)
	}
}
