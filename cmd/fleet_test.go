package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

func TestExtractDroppedImages(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(img, []byte("fakepng"), 0o644); err != nil {
		t.Fatal(err)
	}

	text, images, note := extractDroppedImages("look at " + img + " please")
	if text != "look at [image 1] please" {
		t.Errorf("text = %q", text)
	}
	if len(images) != 1 || !strings.HasPrefix(images[0], "data:image/png;base64,") {
		t.Errorf("images = %v", images)
	}
	if note != "📎 1 image" {
		t.Errorf("note = %q", note)
	}

	// Non-existent path stays in the text.
	text, images, _ = extractDroppedImages("see /nope/missing.png ok")
	if text != "see /nope/missing.png ok" || len(images) != 0 {
		t.Errorf("missing file: text=%q images=%v", text, images)
	}

	// Escaped spaces (iTerm drop) resolve to the real path.
	spaced := filepath.Join(dir, "my shot.jpg")
	if err := os.WriteFile(spaced, []byte("fakejpg"), 0o644); err != nil {
		t.Fatal(err)
	}
	dropped := strings.ReplaceAll(spaced, " ", "\\ ")
	text, images, _ = extractDroppedImages(dropped)
	if text != "[image 1]" || len(images) != 1 || !strings.HasPrefix(images[0], "data:image/jpeg;base64,") {
		t.Errorf("escaped: text=%q images=%d", text, len(images))
	}

	// iTerm single-quotes paths containing spaces.
	text, images, _ = extractDroppedImages("see '" + spaced + "' ok")
	if text != "see [image 1] ok" || len(images) != 1 {
		t.Errorf("quoted: text=%q images=%d", text, len(images))
	}
}

func TestRenderTranscriptCollapsesTools(t *testing.T) {
	s := &gateway.Session{Transcript: []gateway.TranscriptEntry{
		{Role: "user", Content: "list my files"},
		{Role: "assistant", Kind: "tool_use", Content: "Bash: ls -la /Users/x"},
		{Role: "tool", Kind: "tool_result", Content: "line1\nline2\nline3\nline4"},
		{Role: "assistant", Kind: "text", Content: "you have 4 files"},
	}}

	collapsed := strings.Join(renderTranscript(s, 100, false), "\n")
	if strings.Contains(collapsed, "line2") {
		t.Errorf("collapsed view leaks tool payload:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "(+3 lines)") {
		t.Errorf("collapsed view missing line count:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "you have 4 files") {
		t.Errorf("assistant text must stay visible:\n%s", collapsed)
	}

	expanded := strings.Join(renderTranscript(s, 100, true), "\n")
	if !strings.Contains(expanded, "line2") {
		t.Errorf("expanded view must show tool payload:\n%s", expanded)
	}
}
