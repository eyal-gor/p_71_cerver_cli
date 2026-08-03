package cmd

import "testing"

// ↑ walks backwards through what you've sent, ↓ comes forward again and
// hands back the half-written line you were on — the shell contract.
func TestHistoryWalksBackAndReturnsTheDraft(t *testing.T) {
	h := &promptHistory{items: []string{"first", "second", "third"}}
	h.reset()

	for _, want := range []string{"third", "second", "first"} {
		got, ok := h.prev("draft in progress")
		if !ok || got != want {
			t.Fatalf("prev = %q (ok=%v), want %q", got, ok, want)
		}
	}
	// Past the oldest entry it holds, rather than wrapping or emptying.
	if got, ok := h.prev("draft in progress"); ok || got != "draft in progress" {
		t.Errorf("walking past the start should hold; got %q ok=%v", got, ok)
	}

	for _, want := range []string{"second", "third", "draft in progress"} {
		got, ok := h.next()
		if !ok || got != want {
			t.Fatalf("next = %q (ok=%v), want %q", got, ok, want)
		}
	}
	if _, ok := h.next(); ok {
		t.Error("next past the newest entry should report nothing to do")
	}
}

// Sending the same thing twice shouldn't cost two slots.
func TestHistorySkipsImmediateRepeats(t *testing.T) {
	h := &promptHistory{}
	h.add("run the tests")
	h.add("run the tests")
	if len(h.items) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(h.items), h.items)
	}
	h.add("")
	if len(h.items) != 1 {
		t.Errorf("blank input must not be recorded, got %v", h.items)
	}
}

// Browsing then sending starts from the newest again.
func TestSendingResetsThePosition(t *testing.T) {
	h := &promptHistory{items: []string{"a", "b"}}
	h.reset()
	h.prev("")
	h.add("c")
	if h.pos != len(h.items) {
		t.Errorf("position not reset after send: pos=%d len=%d", h.pos, len(h.items))
	}
	if got, _ := h.prev(""); got != "c" {
		t.Errorf("↑ after sending should offer the just-sent prompt, got %q", got)
	}
}

// Multi-line prompts survive the round trip through the on-disk format,
// which is line-based.
func TestMultilinePromptSurvivesEncoding(t *testing.T) {
	h := &promptHistory{}
	h.items = append(h.items, "line one\nline two")
	if got := len(h.items[0]); got == 0 {
		t.Fatal("empty entry")
	}
	// The encoding swaps newlines for NULs on the way out and back.
	encoded := "line one\x00line two"
	decoded := "line one\nline two"
	if enc := replaceAllForTest(h.items[0], "\n", "\x00"); enc != encoded {
		t.Errorf("encode = %q, want %q", enc, encoded)
	}
	if dec := replaceAllForTest(encoded, "\x00", "\n"); dec != decoded {
		t.Errorf("decode = %q, want %q", dec, decoded)
	}
}

func replaceAllForTest(s, old, new string) string {
	out := ""
	for _, r := range s {
		if string(r) == old {
			out += new
			continue
		}
		out += string(r)
	}
	return out
}

// Tests must never touch the real history file. An empty path means
// in-memory only; loadHistory is the only thing that sets a real one.
func TestInMemoryHistoryDoesNotPersist(t *testing.T) {
	h := &promptHistory{} // no path
	h.add("this must not reach disk")
	if h.path != "" {
		t.Fatalf("a bare promptHistory must not carry a path, got %q", h.path)
	}
	if len(h.items) != 1 {
		t.Errorf("it should still record in memory, got %v", h.items)
	}
}

// Scrolling to the oldest lines held is what pulls the previous session in.
func TestNearTopOfTranscript(t *testing.T) {
	const termLines = 40 // viewport of 34
	cases := []struct {
		name   string
		scroll int
		total  int
		want   bool
	}{
		{"pinned to the newest line", 0, 500, false},
		{"halfway up a long transcript", 200, 500, false},
		{"outside the preload window", 460, 500, false},
		{"inside the preload window", 464, 500, true},
		{"exactly at the top", 466, 500, true},
		{"short transcript, nothing to scroll", 0, 10, true},
	}
	for _, c := range cases {
		if got := nearTopOfTranscript(c.scroll, c.total, termLines); got != c.want {
			t.Errorf("%s: nearTopOfTranscript(%d, %d) = %v, want %v",
				c.name, c.scroll, c.total, got, c.want)
		}
	}
}
