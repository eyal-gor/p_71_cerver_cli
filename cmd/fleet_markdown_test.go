package cmd

import (
	"strings"
	"testing"
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
