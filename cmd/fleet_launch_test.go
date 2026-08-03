package cmd

import (
	"strings"
	"testing"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// mkCompute builds a compute reporting the given harnesses and local models.
func mkCompute(tools []string, local map[string][]string) gateway.Compute {
	var c gateway.Compute
	c.Capabilities.CliTools = tools
	c.Capabilities.LocalModels = local
	return c
}

func labels(opts []launchOpt) string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.label
	}
	return strings.Join(out, "\n")
}

// The picker must reflect what the relay reports, not a list baked into the
// CLI — the bug that hid gemma from a relay that had it.
func TestLaunchOptionsFollowTheRelay(t *testing.T) {
	computes := []gateway.Compute{mkCompute([]string{"claude", "gemma"}, nil)}
	got := labels(fleetLaunchOptions(harnessNames(computes), localModels(computes)))

	for _, want := range []string{"claude · default", "gemma · default", "gemma · gemma-4-31b-it"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in picker, got:\n%s", want, got)
		}
	}
	// codex isn't on this relay, so offering it would fail at spawn.
	if strings.Contains(got, "codex") {
		t.Errorf("codex not reported by the relay but shown anyway:\n%s", got)
	}
}

// Ollama's models are files on a specific machine, so they can only come
// from that machine's report — never from a table in this repo.
func TestOllamaModelsComeFromTheMachine(t *testing.T) {
	computes := []gateway.Compute{mkCompute(
		[]string{"ollama"},
		map[string][]string{"ollama": {"llama3.2:1b", "qwen2.5-coder:7b"}},
	)}
	got := labels(fleetLaunchOptions(harnessNames(computes), localModels(computes)))

	for _, want := range []string{"ollama · llama3.2:1b", "ollama · qwen2.5-coder:7b"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q from the machine's inventory, got:\n%s", want, got)
		}
	}
}

// A machine with Ollama installed but nothing pulled can't run anything —
// it should offer the harness's default row and no invented models.
func TestOllamaWithNoModelsPulled(t *testing.T) {
	computes := []gateway.Compute{mkCompute([]string{"ollama"}, nil)}
	opts := fleetLaunchOptions(harnessNames(computes), localModels(computes))
	if len(opts) != 1 || opts[0].cli != "ollama" || opts[0].model != "" {
		t.Errorf("expected only the ollama default row, got: %v", labels(opts))
	}
}

// Models pulled on different machines all remain choosable — the launcher
// picks the compute separately, so a union is right and a per-machine
// intersection would hide models the user actually has.
func TestLocalModelsUnionAcrossComputes(t *testing.T) {
	computes := []gateway.Compute{
		mkCompute([]string{"ollama"}, map[string][]string{"ollama": {"llama3.2:1b"}}),
		mkCompute([]string{"ollama"}, map[string][]string{"ollama": {"deepseek-r1:8b", "llama3.2:1b"}}),
	}
	got := labels(fleetLaunchOptions(harnessNames(computes), localModels(computes)))
	for _, want := range []string{"llama3.2:1b", "deepseek-r1:8b"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in the union, got:\n%s", want, got)
		}
	}
	if strings.Count(got, "llama3.2:1b") != 1 {
		t.Errorf("model reported by two computes should appear once:\n%s", got)
	}
}

// An offline relay (or one too old to send capabilities) must still give a
// usable picker rather than an empty sheet.
func TestEmptyCapabilitiesFallsBack(t *testing.T) {
	opts := fleetLaunchOptions(nil, nil)
	if len(opts) == 0 {
		t.Fatal("picker is empty with no reported capabilities")
	}
	if opts[0].cli != "claude" {
		t.Errorf("expected claude first in the fallback, got %q", opts[0].cli)
	}
}

// The onboarding card answers whatever is actually missing, and goes quiet
// once nothing is. A static welcome message would keep taking space forever.
func TestOnboardCardFollowsTheRelayState(t *testing.T) {
	full := []gateway.Compute{mkCompute(
		[]string{"claude", "codex", "grok"},
		map[string][]string{"ollama": {"llama3.2:1b"}},
	)}
	full[0].Status = "ready"

	// With nothing missing it falls back to a suggestion rather than a nag.
	if card := onboardCard(full, 24); !onboardDismissed() && !containsText(card, "cerver crons") {
		t.Errorf("expected the scheduling suggestion, got:\n%s", strings.Join(card, "\n"))
	}

	var none []gateway.Compute
	card := onboardCard(none, 24)
	if card == nil {
		t.Fatal("with no compute at all the card must offer the install")
	}
	if !containsText(card, "Install the relay") {
		t.Errorf("expected the install prompt, got:\n%s", strings.Join(card, "\n"))
	}

	noLocal := []gateway.Compute{mkCompute([]string{"claude", "codex", "grok"}, nil)}
	noLocal[0].Status = "ready"
	if card := onboardCard(noLocal, 24); !containsText(card, "ollama pull") {
		t.Errorf("with no local models it should suggest pulling one, got:\n%s", strings.Join(card, "\n"))
	}
}

// Every card line must fit the column, or the text is clipped mid-word.
func TestOnboardCardFitsTheColumn(t *testing.T) {
	const width = 24
	for _, computes := range [][]gateway.Compute{
		nil,
		{mkCompute([]string{"claude"}, nil)},
		{mkCompute([]string{"claude", "codex", "grok"}, nil)},
	} {
		for _, ln := range onboardCard(computes, width) {
			if n := len([]rune(stripANSI(ln))); n > width {
				t.Errorf("card line is %d wide, column is %d: %q", n, width, stripANSI(ln))
			}
		}
	}
}

func containsText(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(stripANSI(l), want) {
			return true
		}
	}
	return false
}

// Context size is the sum of uncached + cached input. Reading input_tokens
// alone reports a nearly-empty context on any cached session, which is most
// of them.
func TestContextUsedCountsCachedInput(t *testing.T) {
	s := &gateway.Session{Metadata: map[string]any{
		"usage_last": map[string]any{
			"input_tokens":                float64(3_000),
			"cache_read_input_tokens":     float64(180_000),
			"cache_creation_input_tokens": float64(17_000),
			"output_tokens":               float64(900),
		},
	}}
	if got, want := s.ContextUsed(), 200_000; got != want {
		t.Errorf("ContextUsed() = %d, want %d (input alone would be 3000)", got, want)
	}
}

// usage_total is cumulative across turns and must not be mistaken for context.
func TestContextUsedIgnoresCumulativeTotal(t *testing.T) {
	s := &gateway.Session{Metadata: map[string]any{
		"usage_total": map[string]any{"input_tokens": float64(4_000_000)},
	}}
	if got := s.ContextUsed(); got != 0 {
		t.Errorf("cumulative total must not be read as context; got %d", got)
	}
}

func TestContextWindowKnownAndUnknown(t *testing.T) {
	cases := []struct {
		model string
		want  int
		known bool
	}{
		{"claude-opus-5", 1_000_000, true},
		{"claude-opus-5[1m]", 1_000_000, true},
		{"claude-sonnet-5", 1_000_000, true},
		{"claude-haiku-4-5-20251001", 200_000, true},
		{"gpt-5-codex", 0, false},
		{"llama3.2:1b", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, known := gateway.ContextWindow(c.model)
		if got != c.want || known != c.known {
			t.Errorf("ContextWindow(%q) = (%d, %v), want (%d, %v)",
				c.model, got, known, c.want, c.known)
		}
	}
}
