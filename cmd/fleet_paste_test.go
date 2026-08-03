package cmd

import (
	"strings"
	"testing"
)

// A long or multi-line paste is referenced, not inlined: the input line has
// to stay readable, and what gets sent has to be the real thing.
func TestPasteIsSummarizedButSentInFull(t *testing.T) {
	ps := &pasteStore{}
	body := "line one\nline two\nline three of a much longer thing"

	raw := "look at this: " + ps.add(body)

	shown := ps.display(raw)
	if strings.Contains(shown, "line two") {
		t.Errorf("paste body leaked onto the input line: %q", shown)
	}
	if !strings.Contains(shown, "[pasted text · 11 words]") {
		t.Errorf("expected a word-count summary, got %q", shown)
	}
	if strings.Contains(shown, "\n") {
		t.Errorf("summary must stay on one line, got %q", shown)
	}

	sent := ps.expand(raw)
	if !strings.Contains(sent, body) {
		t.Errorf("the agent must receive the full paste; got %q", sent)
	}
	if !strings.HasPrefix(sent, "look at this: ") {
		t.Errorf("text typed around the paste must survive; got %q", sent)
	}
}

// Newlines in a paste have to reach the agent — that's the multi-line input.
func TestMultilinePasteKeepsItsNewlines(t *testing.T) {
	ps := &pasteStore{}
	raw := ps.add("first\nsecond\nthird")
	if got := strings.Count(ps.expand(raw), "\n"); got != 2 {
		t.Errorf("expected 2 newlines to survive, got %d", got)
	}
}

// Backspace deletes a paste as one unit. Erasing a 200-line paste one
// character at a time would be unusable.
func TestBackspaceRemovesAWholePaste(t *testing.T) {
	ps := &pasteStore{}
	raw := "hi " + ps.add("a\nb")

	after := dropLastUnit(raw)
	if after != "hi " {
		t.Errorf("expected the whole reference removed, got %q", after)
	}
	if got := dropLastUnit("abc"); got != "ab" {
		t.Errorf("ordinary text should lose one rune, got %q", got)
	}
	if got := dropLastUnit(""); got != "" {
		t.Errorf("empty input must stay empty, got %q", got)
	}
}

// Text that merely looks like a marker must not be mistaken for one.
func TestUnknownMarkerIsTreatedAsText(t *testing.T) {
	ps := &pasteStore{}
	raw := "a\x0199\x02b" // index 99 was never stored
	if got := ps.expand(raw); !strings.Contains(got, "99") {
		t.Errorf("a bogus reference should survive as text, got %q", got)
	}
}
