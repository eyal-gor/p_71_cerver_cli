package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Connect wires the machine's coding agents through the cerver gateway.
// After `cerver connect`, Claude Code and Codex route every model request
// via gateway.cerver.ai — metered, capped, attributed — with zero change
// to how the user works. `cerver connect off` restores direct vendor
// access. Subscription logins are untouched until connect is on: setting
// an API key makes the CLIs prefer it over their OAuth login.
//
//	cerver connect            configure Claude Code + Codex
//	cerver connect claude     Claude Code only  (~/.claude/settings.json)
//	cerver connect codex      Codex only        (~/.codex/config.toml)
//	cerver connect status     show what's routed through cerver
//	cerver connect off        restore direct vendor access
//	  --print                 show the changes without writing them
func Connect(args []string) error {
	sub := "all"
	rest := args
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub = args[0]
		rest = args[1:]
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	printOnly := fs.Bool("print", false, "show changes without writing")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	switch sub {
	case "all", "claude", "codex":
		key, err := connectKey()
		if err != nil {
			return err
		}
		if sub == "all" || sub == "claude" {
			if err := connectClaude(key, *printOnly); err != nil {
				return err
			}
		}
		if sub == "all" || sub == "codex" {
			if err := connectCodex(key, *printOnly); err != nil {
				return err
			}
		}
		if !*printOnly {
			fmt.Println("\nDone. New terminals pick this up automatically; current Claude Code")
			fmt.Println("sessions need a restart. Verify with: cerver connect status")
		}
		return nil
	case "off":
		if err := disconnectClaude(*printOnly); err != nil {
			return err
		}
		return disconnectCodex(*printOnly)
	case "status":
		return connectStatus()
	case "help", "-h", "--help":
		fmt.Print(connectHelpText)
		return nil
	default:
		return fmt.Errorf("unknown connect subcommand: %s (try `cerver connect help`)", sub)
	}
}

const connectHelpText = `cerver connect — route your coding agents through the cerver gateway

One command, then you use claude / codex exactly as before. Every request
flows through gateway.cerver.ai: metered per key, capped by your spending
limits, paid with the vendor key in your cerver vault.

  cerver connect            configure Claude Code + Codex
  cerver connect claude     Claude Code only
  cerver connect codex      Codex only
  cerver connect status     what's currently routed
  cerver connect off        restore direct vendor access
    --print                 preview changes without writing

Claude Code: sets env.ANTHROPIC_BASE_URL + ANTHROPIC_API_KEY in
~/.claude/settings.json (backup written alongside on first change).
Codex: adds a "cerver" model provider to ~/.codex/config.toml and exports
CERVER_API_KEY in your shell profile.

Note: with a key configured, the CLIs prefer it over a claude.ai /
chatgpt.com subscription login. "off" returns you to the subscription.
`

func gatewayBase() string {
	if v := os.Getenv("CERVER_GATEWAY_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://gateway.cerver.ai"
}

func connectKey() (string, error) {
	if v := os.Getenv("CERVER_API_KEY"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	key := readEnvKey(filepath.Join(home, ".cerver", "cerver.env"), "CERVER_API_KEY")
	if key == "" {
		return "", fmt.Errorf("no cerver key found — run `cerver login` first")
	}
	return key, nil
}

// ── Claude Code (~/.claude/settings.json) ────────────────────────────────

func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func connectClaude(key string, printOnly bool) error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings := map[string]any{}
	raw, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON — fix it first (%v)", path, err)
		}
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = gatewayBase() + "/v2/proxy/anthropic"
	env["ANTHROPIC_API_KEY"] = key
	settings["env"] = env

	// Bottom-of-terminal indicator: routed or not, provider, model, spend.
	// Installed once; kept on disconnect (it shows "cerver off" then).
	if _, has := settings["statusLine"]; !has {
		settings["statusLine"] = map[string]any{
			"type":    "command",
			"command": "cerver statusline",
		}
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if printOnly {
		fmt.Printf("would write %s:\n  env.ANTHROPIC_BASE_URL = %s\n  env.ANTHROPIC_API_KEY = %s…\n",
			path, gatewayBase()+"/v2/proxy/anthropic", key[:min(8, len(key))])
		return nil
	}
	if readErr == nil {
		backup := path + ".cerver-backup"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			_ = os.WriteFile(backup, raw, 0o600)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("✓ Claude Code → cerver gateway (%s)\n", path)
	return nil
}

func disconnectClaude(printOnly bool) error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("· Claude Code: nothing to disconnect")
		return nil
	}
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("%s is not valid JSON (%v)", path, err)
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		fmt.Println("· Claude Code: nothing to disconnect")
		return nil
	}
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	if !strings.Contains(base, "/v2/proxy/anthropic") {
		fmt.Println("· Claude Code: not routed through cerver")
		return nil
	}
	if printOnly {
		fmt.Printf("would remove env.ANTHROPIC_BASE_URL + env.ANTHROPIC_API_KEY from %s\n", path)
		return nil
	}
	delete(env, "ANTHROPIC_BASE_URL")
	delete(env, "ANTHROPIC_API_KEY")
	if len(env) == 0 {
		delete(settings, "env")
	} else {
		settings["env"] = env
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("✓ Claude Code disconnected — back to direct Anthropic / subscription login")
	return nil
}

// ── Codex (~/.codex/config.toml + shell profile) ─────────────────────────

const codexBlockStart = "# >>> cerver connect >>>"
const codexBlockEnd = "# <<< cerver connect <<<"

func codexConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func stripManagedBlock(content, start, end string) string {
	for {
		i := strings.Index(content, start)
		if i < 0 {
			return content
		}
		j := strings.Index(content[i:], end)
		if j < 0 {
			return content[:i]
		}
		content = content[:i] + strings.TrimLeft(content[i+j+len(end):], "\n")
	}
}

func connectCodex(key string, printOnly bool) error {
	path, err := codexConfigPath()
	if err != nil {
		return err
	}
	existing := ""
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	}
	cleaned := stripManagedBlock(existing, codexBlockStart, codexBlockEnd)

	// TOML: top-level keys must precede tables, so model_provider goes in a
	// managed block at the TOP; the provider table goes at the BOTTOM.
	top := codexBlockStart + "\nmodel_provider = \"cerver\"\n" + codexBlockEnd + "\n"
	bottom := "\n" + codexBlockStart + `
[model_providers.cerver]
name = "Cerver Gateway"
base_url = "` + gatewayBase() + `/v2/proxy/openai"
env_key = "CERVER_API_KEY"
wire_api = "responses"
` + codexBlockEnd + "\n"

	next := top + strings.TrimLeft(cleaned, "\n") + bottom
	if printOnly {
		fmt.Printf("would write %s with a cerver model provider (base_url %s)\n", path, gatewayBase()+"/v2/proxy/openai")
		return shellExportCerverKey(key, true)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		return err
	}
	fmt.Printf("✓ Codex → cerver gateway (%s)\n", path)
	return shellExportCerverKey(key, false)
}

func disconnectCodex(printOnly bool) error {
	path, err := codexConfigPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("· Codex: nothing to disconnect")
		return nil
	}
	if !strings.Contains(string(raw), codexBlockStart) {
		fmt.Println("· Codex: not routed through cerver")
		return nil
	}
	if printOnly {
		fmt.Printf("would remove the cerver blocks from %s\n", path)
		return nil
	}
	cleaned := strings.TrimLeft(stripManagedBlock(string(raw), codexBlockStart, codexBlockEnd), "\n")
	if err := os.WriteFile(path, []byte(cleaned), 0o600); err != nil {
		return err
	}
	fmt.Println("✓ Codex disconnected — back to direct OpenAI / subscription login")
	return nil
}

// Codex reads the key from the environment (env_key), so export it in the
// shell profile inside a managed block.
func shellExportCerverKey(key string, printOnly bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	profile := filepath.Join(home, ".zshrc")
	if shell := os.Getenv("SHELL"); strings.Contains(shell, "bash") {
		profile = filepath.Join(home, ".bashrc")
	}
	if printOnly {
		fmt.Printf("would export CERVER_API_KEY in %s\n", profile)
		return nil
	}
	existing := ""
	if raw, err := os.ReadFile(profile); err == nil {
		existing = string(raw)
	}
	cleaned := stripManagedBlock(existing, codexBlockStart, codexBlockEnd)
	block := "\n" + codexBlockStart + "\nexport CERVER_API_KEY=\"" + key + "\"\n" + codexBlockEnd + "\n"
	if err := os.WriteFile(profile, []byte(strings.TrimRight(cleaned, "\n")+"\n"+block), 0o600); err != nil {
		return err
	}
	fmt.Printf("✓ CERVER_API_KEY exported in %s (new terminals only)\n", profile)
	return nil
}

// ── status ────────────────────────────────────────────────────────────────

func connectStatus() error {
	claudePath, _ := claudeSettingsPath()
	claudeState := "direct (vendor / subscription)"
	if raw, err := os.ReadFile(claudePath); err == nil {
		var s struct {
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal(raw, &s) == nil && strings.Contains(s.Env["ANTHROPIC_BASE_URL"], "/v2/proxy/anthropic") {
			claudeState = "→ cerver gateway"
		}
	}
	codexPath, _ := codexConfigPath()
	codexState := "direct (vendor / subscription)"
	if raw, err := os.ReadFile(codexPath); err == nil && strings.Contains(string(raw), codexBlockStart) {
		codexState = "→ cerver gateway"
	}
	fmt.Printf("Claude Code  %s\n", claudeState)
	fmt.Printf("Codex        %s\n", codexState)
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
