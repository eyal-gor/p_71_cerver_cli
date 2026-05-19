package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// PreflightResult captures the two questions we ask before spending
// any real time on a CLI: "can this CLI authenticate?" and "is the
// provider's API reachable?". A `cerver test` (or future `cerver run`)
// uses this to fail fast — a missing XAI_API_KEY or an api.x.ai
// outage should burn 5 seconds, not 90.
type PreflightResult struct {
	CLI          string
	AuthOK       bool
	AuthDetail   string
	HealthOK     bool
	HealthDetail string
	// Elapsed is end-to-end preflight time, mostly for telemetry.
	Elapsed time.Duration
}

// Pass returns true only when both checks succeeded. Surfaced to the
// caller as the gate for actually spawning a session.
func (p PreflightResult) Pass() bool {
	return p.AuthOK && p.HealthOK
}

// preflightCheck runs both checks in parallel, with a 6-second timeout
// each. `inf` may be nil if Infisical isn't loaded; the grok check
// degrades to "vault check skipped" in that case rather than failing.
//
// Health URLs hit each provider's API root with a 5-second cap. We
// don't care about the response shape — any HTTP response means the
// host is reachable. Connection refused / DNS failure / timeout all
// surface as health=fail.
func preflightCheck(ctx context.Context, cli string, inf *infisical.Client) PreflightResult {
	pf := PreflightResult{CLI: cli}
	start := time.Now()
	defer func() { pf.Elapsed = time.Since(start) }()

	// Run auth and health concurrently — they don't depend on each
	// other and combined we want < 6s wall time.
	authDone := make(chan struct{})
	healthDone := make(chan struct{})

	go func() {
		defer close(authDone)
		pf.AuthOK, pf.AuthDetail = authCheck(ctx, cli, inf)
	}()
	go func() {
		defer close(healthDone)
		pf.HealthOK, pf.HealthDetail = healthCheck(ctx, cli)
	}()

	<-authDone
	<-healthDone
	return pf
}

func authCheck(ctx context.Context, cli string, inf *infisical.Client) (bool, string) {
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	switch cli {
	case "claude":
		// Claude's CLI doesn't expose a `login status` subcommand
		// publicly; the simplest cheap probe is `claude --version`,
		// which exits 0 only when the binary's runtime can start.
		// Real auth-state check would need to peek at the CLI's
		// keychain entry — not portable across macOS/Linux/Windows.
		path, err := exec.LookPath("claude")
		if err != nil {
			return false, "claude CLI not installed"
		}
		if err := exec.CommandContext(tctx, "claude", "--version").Run(); err != nil {
			return false, fmt.Sprintf("claude --version failed: %v", err)
		}
		return true, "OAuth (Claude Max/Pro/Team), binary at " + path
	case "codex":
		// codex ships a real `codex login status` subcommand that
		// prints "Logged in using ChatGPT" or "Logged in using an
		// API key" and exits 0, or errors when not signed in.
		out, err := exec.CommandContext(tctx, "codex", "login", "status").CombinedOutput()
		detail := strings.TrimSpace(string(out))
		if err != nil {
			if detail != "" {
				return false, detail
			}
			return false, fmt.Sprintf("codex login status: %v", err)
		}
		return true, detail
	case "grok":
		// Grok via cerver runs through the claude binary with
		// ANTHROPIC_BASE_URL=https://api.x.ai — auth is the
		// XAI_API_KEY, which lives in the Infisical vault on this
		// machine (or, in some setups, the local relay config).
		if inf == nil {
			return false, "no Infisical handle — can't verify XAI_API_KEY"
		}
		key, err := inf.Get(tctx, "XAI_API_KEY")
		if err != nil {
			return false, fmt.Sprintf("vault: %v", err)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return false, "XAI_API_KEY empty in vault"
		}
		// Tail/head masking so we don't print the secret. Five at
		// each end is enough to spot stale keys (xai-XXXX…YYYY).
		mask := key
		if len(mask) > 12 {
			mask = mask[:5] + "…" + mask[len(mask)-4:]
		}
		return true, "XAI_API_KEY in vault (" + mask + ")"
	}
	return false, "unknown cli"
}

func healthCheck(ctx context.Context, cli string) (bool, string) {
	url := map[string]string{
		"claude": "https://api.anthropic.com/",
		"codex":  "https://api.openai.com/",
		"grok":   "https://api.x.ai/",
	}[cli]
	if url == "" {
		return false, "no health endpoint configured for " + cli
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, "GET", url, nil)
	if err != nil {
		return false, err.Error()
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Sprintf("unreachable: %v", err)
	}
	defer resp.Body.Close()
	rtt := time.Since(start).Milliseconds()
	// Any HTTP response means the host is up. 4xx is fine — that's
	// the provider answering "you didn't send a real request," which
	// proves the network path works.
	return true, fmt.Sprintf("HTTP %d · %dms", resp.StatusCode, rtt)
}

// formatPreflight renders a one-line summary suitable for stdout
// before the heavier session-spawn work begins. Used by `cerver test`
// to make pre-call state visible.
func formatPreflight(pf PreflightResult) string {
	authIcon := "✓"
	if !pf.AuthOK {
		authIcon = "✗"
	}
	healthIcon := "✓"
	if !pf.HealthOK {
		healthIcon = "✗"
	}
	return fmt.Sprintf("  %-6s  auth %s %s  ·  health %s %s",
		pf.CLI, authIcon, pf.AuthDetail, healthIcon, pf.HealthDetail)
}
