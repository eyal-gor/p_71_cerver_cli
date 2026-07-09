package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	projectFlag := fs.String("project", "", "bind routed traffic to this project (mints/reuses a project key)")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	switch sub {
	case "all", "claude", "codex":
		// A global key can't scope cost/caps/redaction — routing needs a
		// PROJECT key. Mint/reuse one when --project is given; otherwise
		// require that a project routing key is already bound.
		if *projectFlag != "" && !*printOnly {
			if err := bindProjectRoutingKey(*projectFlag); err != nil {
				return err
			}
		}
		key, err := routingKey()
		if err != nil {
			return err
		}
		if _, scope := connectWhoami(); scope == "global" && !*printOnly {
			return fmt.Errorf("your key is GLOBAL — routing needs a project key.\n" +
				"Run:  cerver connect --project <name>\n" +
				"(mints/reuses a project key so cost, caps and redaction apply)")
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
			proj, scope := connectWhoami()
			if scope == "project" {
				fmt.Printf("\nRouted traffic lands in project: %s (cost, caps & redaction apply here).\n", proj)
			} else if scope == "global" {
				fmt.Println("\nHeads up: your key is GLOBAL — traffic isn't scoped to a project, so")
				fmt.Println("per-project cost/caps/redaction won't apply. Bind a project key to fix that.")
			}
			fmt.Println("\nDone. New terminals pick this up automatically; current Claude Code")
			fmt.Println("sessions need a restart. Verify with: cerver connect status")
		}
		return nil
	case "off":
		if err := disconnectClaude(*printOnly); err != nil {
			return err
		}
		if err := disconnectCodex(*printOnly); err != nil {
			return err
		}
		return removeShellShims(*printOnly)
	case "status":
		return connectStatus()
	case "help", "-h", "--help":
		fmt.Print(connectHelpText)
		return nil
	default:
		return fmt.Errorf("unknown connect subcommand: %s (try `cerver connect help`)", sub)
	}
}

const connectHelpText = `cerver connect — one-time setup; your subscription stays the default

Installs the terminal indicator and invisible claude/codex shims. Nothing
about your daily work changes. When a subscription walls you ("usage limit
reached"), type:

  cerver bridge             new claude/codex launches route via the cerver
                            gateway (your vault key, any provider) until
  cerver bridge off         you switch back — or the limit resets

Commands:
  cerver connect            set up Claude Code + Codex
  cerver connect claude     Claude Code only (statusline indicator)
  cerver connect codex      Codex only (cerver profile, default unchanged)
  cerver connect status     what's installed / routed
  cerver connect off        remove everything cerver added
    --print                 preview changes without writing
`

func gatewayBase() string {
	if v := os.Getenv("CERVER_GATEWAY_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://gateway.cerver.ai"
}

func routingKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cerver", "gateway.key")
}

// adminKey is the account/global key used to MINT project keys and manage
// the account (from cerver.env / env). Not used for routing.
func adminKey() (string, error) {
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

// routingKey is the key gateway traffic actually uses: the bound project
// key (~/.cerver/gateway.key) if present, else the admin key as fallback.
func routingKey() (string, error) {
	if b, err := os.ReadFile(routingKeyPath()); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
	}
	return adminKey()
}

// connectKey kept as an alias for callers that want the routing key.
func connectKey() (string, error) { return routingKey() }

// bindProjectRoutingKey mints (or reuses) a project-bound key for slug via
// the gateway, using the admin key, and stores it as the routing key.
func bindProjectRoutingKey(slug string) error {
	admin, err := adminKey()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"project_slug": slug,
		"label":        "gateway-" + slug,
		"reuse":        true,
	})
	req, _ := http.NewRequest("POST", gatewayBase()+"/v2/auth/keys", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("minting project key failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("minting project key for %q failed (HTTP %d) — does the project exist on your account?", slug, resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Key == "" {
		return fmt.Errorf("minting project key: unexpected response")
	}
	if err := os.MkdirAll(filepath.Dir(routingKeyPath()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(routingKeyPath(), []byte(out.Key+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("✓ Bound routed traffic to project: %s\n", slug)
	return nil
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
	// NO env flip here: the subscription stays the default. Routing happens
	// per-launch via the shell shim when bridge mode is on. Connect only
	// installs the bottom-of-terminal indicator.
	if _, has := settings["statusLine"]; !has {
		settings["statusLine"] = map[string]any{
			"type":    "command",
			"command": "cerver statusline",
		}
	}
	_ = key

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if printOnly {
		fmt.Printf("would install statusLine (\"cerver statusline\") in %s\n", path)
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
	fmt.Printf("✓ Claude Code: cerver indicator installed (%s)\n", path)
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
	sl, _ := settings["statusLine"].(map[string]any)
	cmdStr, _ := sl["command"].(string)
	if !strings.Contains(cmdStr, "cerver statusline") {
		fmt.Println("· Claude Code: nothing cerver-managed to remove")
		return nil
	}
	if printOnly {
		fmt.Printf("would remove the cerver statusLine from %s\n", path)
		return nil
	}
	delete(settings, "statusLine")
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("✓ Claude Code: cerver statusline removed")
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

	// Provider + profile tables only — NO top-level model_provider, so the
	// default stays the user's subscription. Bridge launches use
	// `codex --profile cerver` via the shell shim.
	bottom := "\n" + codexBlockStart + `
[model_providers.cerver]
name = "Cerver Gateway"
base_url = "` + gatewayBase() + `/v2/proxy/openai"
env_key = "CERVER_API_KEY"
wire_api = "responses"

[profiles.cerver]
model_provider = "cerver"
` + codexBlockEnd + "\n"

	next := strings.TrimLeft(cleaned, "\n") + bottom
	if printOnly {
		fmt.Printf("would add a cerver provider profile to %s (default stays direct)\n", path)
		return installShellShims(true)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		return err
	}
	fmt.Printf("✓ Codex: cerver profile added (%s) — default unchanged\n", path)
	_ = key
	return installShellShims(false)
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

// Shell shims: transparent claude()/codex() functions in the profile.
// Default launches are untouched; when ~/.cerver/bridge exists they route
// through the gateway. The user never touches an env var.
func shellShimBlock() string {
	gw := gatewayBase()
	return codexBlockStart + `
_cerver_routing_key() {
  cat "$HOME/.cerver/gateway.key" 2>/dev/null || \
    grep '^CERVER_API_KEY=' "$HOME/.cerver/cerver.env" 2>/dev/null | cut -d= -f2-
}
claude() {
  if [ -f "$HOME/.cerver/bridge" ]; then
    ANTHROPIC_BASE_URL="` + gw + `/v2/proxy/anthropic" \
    ANTHROPIC_API_KEY="$(_cerver_routing_key)" \
    command claude "$@"
  else
    command claude "$@"
  fi
}
codex() {
  if [ -f "$HOME/.cerver/bridge-codex" ]; then
    CERVER_API_KEY="$(_cerver_routing_key)" \
    command codex --profile cerver "$@"
  else
    command codex "$@"
  fi
}
` + codexBlockEnd + "\n"
}

func shellProfilePath() string {
	home, _ := os.UserHomeDir()
	if shell := os.Getenv("SHELL"); strings.Contains(shell, "bash") {
		return filepath.Join(home, ".bashrc")
	}
	return filepath.Join(home, ".zshrc")
}

func installShellShims(printOnly bool) error {
	profile := shellProfilePath()
	if printOnly {
		fmt.Printf("would install claude/codex bridge shims in %s\n", profile)
		return nil
	}
	existing := ""
	if raw, err := os.ReadFile(profile); err == nil {
		existing = string(raw)
	}
	cleaned := stripManagedBlock(existing, codexBlockStart, codexBlockEnd)
	next := strings.TrimRight(cleaned, "\n") + "\n\n" + shellShimBlock()
	if err := os.WriteFile(profile, []byte(next), 0o600); err != nil {
		return err
	}
	fmt.Printf("✓ bridge shims installed in %s (new terminals)\n", profile)
	return nil
}

func removeShellShims(printOnly bool) error {
	profile := shellProfilePath()
	raw, err := os.ReadFile(profile)
	if err != nil {
		return nil
	}
	if !strings.Contains(string(raw), codexBlockStart) {
		return nil
	}
	if printOnly {
		fmt.Printf("would remove bridge shims from %s\n", profile)
		return nil
	}
	cleaned := strings.TrimRight(stripManagedBlock(string(raw), codexBlockStart, codexBlockEnd), "\n") + "\n"
	if err := os.WriteFile(profile, []byte(cleaned), 0o600); err != nil {
		return err
	}
	fmt.Println("✓ bridge shims removed")
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

	// Where does routed traffic land? Cost, caps and redaction are all
	// per-project, so this is the important line.
	proj, scope := connectWhoami()
	if scope == "project" {
		fmt.Printf("Project      %s  (cost, caps & redaction apply here)\n", proj)
	} else if scope == "global" {
		fmt.Println("Project      (global key — traffic is account-wide, not scoped to a project)")
		fmt.Println("             → bind a project key for per-project cost/caps/redaction:")
		fmt.Println("               cerver keys create --project <name>, then re-run cerver login with it")
	}
	return nil
}

// connectWhoami returns (projectSlug, scope) for the active key via the
// gateway. scope is "project" | "global" | "" (unreachable).
func connectWhoami() (string, string) {
	key, err := connectKey()
	if err != nil || key == "" {
		return "", ""
	}
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", gatewayBase()+"/v2/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var out struct {
		ProjectSlug string `json:"project_slug"`
		Scope       string `json:"scope"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", ""
	}
	return out.ProjectSlug, out.Scope
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
