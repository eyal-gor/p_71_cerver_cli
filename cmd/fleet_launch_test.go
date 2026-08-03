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
