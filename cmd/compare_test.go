package cmd

import "testing"

func TestSplitCLIModel(t *testing.T) {
	cases := []struct {
		in        string
		cli, mode string
	}{
		{"claude", "claude", ""},
		{"claude/opus-4.8", "claude", "opus-4.8"},
		{"claude/opus-4.7", "claude", "opus-4.7"},
		{"codex/gpt-5-codex", "codex", "gpt-5-codex"},
		{" claude / opus ", "claude", "opus"},                         // trims around the slash
		{"openai/anthropic/claude-x", "openai", "anthropic/claude-x"}, // split on first slash only
	}
	for _, c := range cases {
		cli, mode := splitCLIModel(c.in)
		if cli != c.cli || mode != c.mode {
			t.Errorf("splitCLIModel(%q) = (%q,%q), want (%q,%q)", c.in, cli, mode, c.cli, c.mode)
		}
	}
}

func TestLegLabel(t *testing.T) {
	if got := legLabel("claude", "opus-4.8", "mac-mini"); got != "claude/opus-4.8@mac-mini" {
		t.Errorf("legLabel with model = %q", got)
	}
	if got := legLabel("claude", "", "mac-mini"); got != "claude@mac-mini" {
		t.Errorf("legLabel without model = %q", got)
	}
}
