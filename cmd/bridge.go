package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Bridge toggles bridge mode: while on, every NEW claude/codex launch on
// this machine routes through the cerver gateway (via the shell shims that
// `cerver connect` installed) — paid by the vendor key in your cerver
// vault, metered and capped. The moment your subscription walls you:
//
//	cerver bridge         → keep working in the same tools
//	cerver bridge off     → back to the subscription (or wait for reset)
//
// The flag is a file (~/.cerver/bridge) so the shims, the statusline, and
// any other tool can read it without IPC.
func Bridge(args []string) error {
	sub := "on"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub = args[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flagPath := filepath.Join(home, ".cerver", "bridge")             // claude → gateway
	codexFlagPath := filepath.Join(home, ".cerver", "bridge-codex")  // codex → gateway (opt-in)

	switch sub {
	case "on":
		if err := os.MkdirAll(filepath.Dir(flagPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(flagPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("⚡ bridge ON — new claude/codex launches route via the cerver gateway.")
		fmt.Println("  Already-open sessions keep their current routing; restart them to bridge.")
		fmt.Println()
		fmt.Println("  Your options while the subscription is walled:")
		fmt.Println("    claude               same tool, now on your vault key via Cerver (pay per token)")
		fmt.Println("    codex                have a ChatGPT plan? switch tools — runs DIRECT on that
                         plan, flat-rate, no gateway needed")
		fmt.Println("    cerver bridge off    when the limit resets, go back to the subscription")
		return nil
	case "codex":
		// Route CODEX through the gateway too (per-token). Only needed when
		// codex ITSELF is walled — otherwise leave it on its flat-rate plan.
		if err := os.MkdirAll(filepath.Dir(codexFlagPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(codexFlagPath, []byte("on\n"), 0o600); err != nil {
			return err
		}
		fmt.Println("⚡ codex now routes via the cerver gateway too (per-token). New launches only.")
		return nil
	case "off":
		_ = os.Remove(codexFlagPath)
		if err := os.Remove(flagPath); err != nil {
			if os.IsNotExist(err) {
				fmt.Println("· bridge already off")
				return nil
			}
			return err
		}
		fmt.Println("○ bridge OFF — new launches go direct on your subscription again.")
		return nil
	case "status":
		if st, err := os.Stat(flagPath); err == nil {
			fmt.Printf("⚡ bridge ON since %s — new launches route via cerver\n", st.ModTime().Format("15:04"))
		} else {
			fmt.Println("○ bridge off — launches go direct on your subscription")
		}
		return nil
	case "help", "-h", "--help":
		fmt.Print(bridgeHelpText)
		return nil
	default:
		return fmt.Errorf("unknown bridge subcommand: %s (try on|off|status)", sub)
	}
}

const bridgeHelpText = `cerver bridge — keep working when your subscription hits its limit

  cerver bridge          ON: new claude/codex launches route via the cerver
                         gateway, paid by the vendor key in your vault
  cerver bridge off      back to your subscription
  cerver bridge status   which mode this machine is in

Needs the one-time setup: cerver connect
`
