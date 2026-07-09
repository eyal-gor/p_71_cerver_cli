package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Gateway toggles whether Claude Code's traffic routes THROUGH Cerver.
//
//	cerver gateway on      route through Cerver (metered, capped, redacted)
//	cerver gateway off     go direct to the vendor on your subscription
//	cerver gateway         show state + a one-line explanation
//	cerver gateway help    what "on" vs "off" actually means
//	cerver gateway codex   also route Codex through the gateway (opt-in)
//
// The switch is a flag file the local bridge daemon reads per request, so it
// takes effect on your next message — no restart. `cerver bridge` is kept as
// an alias for muscle memory.
func Gateway(args []string) error {
	sub := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub = args[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flagPath := filepath.Join(home, ".cerver", "bridge")            // claude → gateway
	codexFlagPath := filepath.Join(home, ".cerver", "bridge-codex") // codex → gateway (opt-in)

	switch sub {
	case "on":
		if err := os.MkdirAll(filepath.Dir(flagPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(flagPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("⚡ Cerver gateway ON — Claude Code now routes through Cerver.")
		fmt.Println("   Takes effect on your next message (no restart). Cost, spend caps and")
		fmt.Println("   your redaction policy apply. Billed per-token on the project's key.")
		fmt.Println("   Back to your subscription: cerver gateway off")
		return nil
	case "off":
		_ = os.Remove(codexFlagPath)
		if err := os.Remove(flagPath); err != nil {
			if os.IsNotExist(err) {
				fmt.Println("· Cerver gateway already off — you're direct on your subscription.")
				return nil
			}
			return err
		}
		fmt.Println("○ Cerver gateway OFF — back on your subscription, from your next message. No restart.")
		return nil
	case "codex":
		if err := os.MkdirAll(filepath.Dir(codexFlagPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(codexFlagPath, []byte("on\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("⚡ Codex now routes through the Cerver gateway too (per-token). Next launch.")
		return nil
	case "status", "":
		on := false
		if _, err := os.Stat(flagPath); err == nil {
			on = true
		}
		if on {
			fmt.Println("⚡ Cerver gateway: ON — Claude Code routes through Cerver (metered, capped, redacted).")
		} else {
			fmt.Println("○ Cerver gateway: OFF — direct to the vendor on your subscription (flat-rate).")
		}
		fmt.Println("   Flip: cerver gateway on|off   ·   What this means: cerver gateway help")
		return nil
	case "help", "-h", "--help":
		fmt.Print(gatewayHelpText)
		return nil
	default:
		return fmt.Errorf("unknown gateway subcommand: %s (try on | off | status | help)", sub)
	}
}

// Bridge is a backward-compatible alias for Gateway.
func Bridge(args []string) error { return Gateway(args) }

const gatewayHelpText = `cerver gateway — route Claude Code through Cerver, or go direct

Two states, one switch (flips on your next message, no restart):

  OFF (direct)
    Your request goes straight to the vendor on your subscription.
    Flat-rate. Cerver isn't in the path — nothing metered, nothing filtered.

  ON (gateway)
    Your request goes through Cerver first, then to the vendor. You get:
      • Cost metering   — every token attributed to your project
      • Spend caps      — a runaway stops before it bills
      • Redaction       — secrets & names you've listed are stripped out
    Billed per-token on the project's own API key (from your vault), not
    your flat-rate subscription. Cerver meters the traffic; it does NOT
    store your prompts.

Commands
  cerver gateway on          route through Cerver
  cerver gateway off         go direct (subscription)
  cerver gateway             show current state
  cerver gateway codex       also route Codex through the gateway (opt-in)

The bottom bar in Claude Code shows which state you're in:
  Cerver · direct · <model> · Project: <p>          off, on your plan
  Cerver · ⚡ gateway · <model> · Project: <p> · $x  on, routed via Cerver

Setup once with: cerver connect --project <name>
`
