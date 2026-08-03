package cmd

import (
	"strings"
	"testing"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

func TestMarkdownIsStyledNotShownAsSource(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantGone   []string // markup the reader should never see
		wantKept   []string // content that must survive
		wantStyled bool
	}{
		{"bold", "a **strong** point", []string{"**"}, []string{"strong"}, true},
		{"code span", "run `go build` now", []string{"`"}, []string{"go build"}, true},
		{"heading", "## Contribution Process", []string{"##"}, []string{"Contribution Process"}, true},
		{"bullet", "- first item", []string{"- "}, []string{"• ", "first item"}, false},
		{"link", "see [the docs](https://x.dev)", []string{"](", "[the"}, []string{"the docs", "https://x.dev"}, true},
	}
	for _, c := range cases {
		got := styleMarkdown(c.in)
		for _, gone := range c.wantGone {
			if strings.Contains(stripANSI(got), gone) {
				t.Errorf("%s: markup %q still visible in %q", c.name, gone, stripANSI(got))
			}
		}
		for _, kept := range c.wantKept {
			if !strings.Contains(stripANSI(got), kept) {
				t.Errorf("%s: lost content %q; got %q", c.name, kept, stripANSI(got))
			}
		}
		if c.wantStyled && !strings.Contains(got, "\x1b[") {
			t.Errorf("%s: expected styling, got plain %q", c.name, got)
		}
	}
}

// Styling must not change how wide a line is, or wrapping breaks.
func TestStylingDoesNotWidenTheLine(t *testing.T) {
	in := "a **bold** and `code` line"
	if got, want := len([]rune(stripANSI(styleMarkdown(in)))), len([]rune("a bold and code line")); got != want {
		t.Errorf("visible width %d, want %d", got, want)
	}
}

// Ordinary prose must come through untouched — no accidental styling.
func TestPlainTextIsLeftAlone(t *testing.T) {
	in := "the process exited with status 1, which is not markdown"
	if got := styleMarkdown(in); got != in {
		t.Errorf("plain text was altered:\n got %q\nwant %q", got, in)
	}
}

// A run of tool calls collapses to one line, not one line per call.
func TestToolCallsCollapseToOneLine(t *testing.T) {
	s := &gateway.Session{Transcript: []gateway.TranscriptEntry{
		{Role: "user", Kind: "text", Content: "audit the auth flow"},
		{Role: "assistant", Kind: "thinking", Content: "considering"},
		{Role: "assistant", Kind: "tool_use", ToolName: "grep", Content: ""},
		{Role: "assistant", Kind: "tool_result", Content: "40 matches"},
		{Role: "assistant", Kind: "tool_use", ToolName: "read", Content: ""},
		{Role: "assistant", Kind: "tool_result", Content: "…file…"},
		{Role: "assistant", Kind: "text", Content: "Found it."},
	}}
	lines := renderTranscript(s, 100, "claude code")
	joined := stripANSI(strings.Join(lines, "\n"))

	if n := strings.Count(joined, "⋯"); n != 1 {
		t.Errorf("expected exactly one collapsed line, got %d:\n%s", n, joined)
	}
	if !strings.Contains(joined, "read") {
		t.Errorf("collapsed line should name the last thing it did:\n%s", joined)
	}
	if !strings.Contains(joined, "3 steps") {
		t.Errorf("expected a step count (thinking + 2 tools), got:\n%s", joined)
	}
	for _, leaked := range []string{"40 matches", "…file…", "considering"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("tool machinery %q leaked into the transcript:\n%s", leaked, joined)
		}
	}
	if !strings.Contains(joined, "Found it.") {
		t.Errorf("the agent's actual answer must survive:\n%s", joined)
	}
}

// The live line reports the newest step, and goes quiet once it has spoken.
func TestLatestActivity(t *testing.T) {
	working := &gateway.Session{Transcript: []gateway.TranscriptEntry{
		{Role: "assistant", Kind: "text", Content: "on it"},
		{Role: "assistant", Kind: "tool_use", ToolName: "grep"},
		{Role: "assistant", Kind: "tool_use", ToolName: "read"},
	}}
	if got := latestActivity(working); got != "read" {
		t.Errorf("latestActivity = %q, want %q", got, "read")
	}

	spoken := &gateway.Session{Transcript: []gateway.TranscriptEntry{
		{Role: "assistant", Kind: "tool_use", ToolName: "read"},
		{Role: "assistant", Kind: "text", Content: "Here's what I found."},
	}}
	if got := latestActivity(spoken); got != "" {
		t.Errorf("after speaking there is nothing in flight; got %q", got)
	}
}
