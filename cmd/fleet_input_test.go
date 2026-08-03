package cmd

import "testing"

// A burst of mouse reports does not arrive on read boundaries. Anything held
// back must be exactly the unfinished tail — hold back too much and keys are
// lost, too little and the fragment is typed into the input bar as junk.
func TestIncompleteEscapeHoldsBackOnlyThePartialTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"plain text", "hello", 0},
		{"whole mouse report", "\x1b[<35;10;5M", 0},
		{"whole arrow key", "\x1b[A", 0},
		{"two whole reports", "\x1b[<35;1;1M\x1b[<35;2;2M", 0},
		{"split mid-report", "\x1b[<35;10", 8},
		{"split right after esc", "text\x1b", 1},
		{"split after bracket", "text\x1b[", 2},
		{"whole report then partial", "\x1b[<0;1;1M\x1b[<35;9", 7},
		{"bare esc is complete", "\x1b", 1},
	}
	for _, c := range cases {
		if got := incompleteEscape([]byte(c.in)); got != c.want {
			t.Errorf("%s: incompleteEscape(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

// The specific failure the user saw: a report split across two reads must
// reassemble into one sequence, never into typed characters.
func TestSplitReportReassembles(t *testing.T) {
	first := []byte("\x1b[<35;10")
	second := []byte(";5M")

	held := incompleteEscape(first)
	if held != len(first) {
		t.Fatalf("held back %d of %d bytes; the rest would be typed as text", held, len(first))
	}
	joined := append(append([]byte{}, first...), second...)
	if n := incompleteEscape(joined); n != 0 {
		t.Errorf("reassembled sequence still looks partial (%d bytes held)", n)
	}
	if !mouseSeq.Match(joined) {
		t.Errorf("reassembled bytes %q do not parse as a mouse report", joined)
	}
}
