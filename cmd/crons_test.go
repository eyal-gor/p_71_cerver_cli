package cmd

import "testing"

// The gateway replaces a cron's config wholesale on PATCH, so an update that
// touches one config field has to resend the rest or they're dropped.
func TestCronConfigCarry(t *testing.T) {
	cur := map[string]any{
		"compute_id": "mac-mini",
		"harness":    "claude",
		"model":      "claude-opus-5",
		"unrelated":  "keep-out",
		"method":     nil,
	}
	patch := cronConfigCarry(map[string]any{"schedule": "0 9 * * *"}, cur)

	if patch["compute_id"] != "mac-mini" || patch["harness"] != "claude" {
		t.Errorf("config not carried over: %v", patch)
	}
	if patch["schedule"] != "0 9 * * *" {
		t.Errorf("carry clobbered the caller's patch: %v", patch)
	}
	if _, ok := patch["unrelated"]; ok {
		t.Errorf("carried a key the gateway doesn't read: %v", patch)
	}
	if _, ok := patch["method"]; ok {
		t.Errorf("carried a nil config value: %v", patch)
	}
}

func TestCronConfigCarryOverriddenByNewFlags(t *testing.T) {
	cur := map[string]any{"compute_id": "mac-mini", "model": "claude-sonnet-5"}
	patch := cronConfigCarry(map[string]any{}, cur)
	patch["model"] = "claude-opus-5" // what --model does after the carry

	if patch["model"] != "claude-opus-5" {
		t.Errorf("new --model should win, got %v", patch["model"])
	}
	if patch["compute_id"] != "mac-mini" {
		t.Errorf("untouched config should survive, got %v", patch["compute_id"])
	}
}
