package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
	"github.com/eyal-gor/p_71_cerver_cli/internal/output"
)

// Run executes a single prompt on a single CLI. Default cli=claude,
// default compute=first local-relay or whatever the user pinned.
func Run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cli := fs.String("cli", "claude", "CLI to run: claude | codex | grok")
	on := fs.String("on", "", "Compute name or id to run on (default: first local relay)")
	bill := fs.String("bill", "", "Billing mode: subscription | api (alias: sub | api)")
	timeoutSec := fs.Int("timeout", 180, "Max seconds to wait for the reply")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return errors.New("usage: cerver run [flags] \"your prompt\"")
	}

	mode, err := resolveBillingMode(*cli, *bill)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*timeoutSec)*time.Second+10*time.Second)
	defer cancel()

	// 1. Unlock cerver token (and api key if mode==api).
	icfg, err := infisical.LoadConfig()
	if err != nil {
		return err
	}
	inf := infisical.New(icfg)
	cerverTok, err := inf.Get(ctx, "CERVER_API_TOKEN")
	if err != nil {
		return err
	}
	if cerverTok == "" {
		return errors.New("CERVER_API_TOKEN not in Infisical")
	}

	envInject := map[string]string{}
	if mode == "api" {
		keyName := apiKeyEnvFor(*cli)
		v, err := inf.Get(ctx, keyName)
		if err != nil {
			return err
		}
		if v == "" {
			return fmt.Errorf("%s set to api but %s isn't in your vault — paste one or use --bill sub", *cli, keyName)
		}
		envInject[keyName] = v
	}

	gw := gateway.New(cerverTok)

	// 2. Pick compute.
	computeID, err := pickCompute(ctx, gw, *on)
	if err != nil {
		return err
	}

	// 3. Create the session.
	metadata := map[string]any{"cli_tool": *cli}
	if len(envInject) > 0 {
		metadata["env"] = envInject
	}
	sessionID, err := gw.CreateSession(ctx, gateway.SessionCreate{
		SessionType: "coding",
		Compute:     map[string]any{"compute_id": computeID},
		Task:        prompt,
		Workload:    "coding",
		SessionName: "cli-run",
		Metadata:    metadata,
	})
	if err != nil {
		return err
	}

	// 4. Kick off the CLI.
	if err := gw.SendInput(ctx, sessionID, prompt); err != nil {
		return err
	}

	// 5. Poll for the assistant reply.
	start := time.Now()
	for {
		if time.Since(start) > time.Duration(*timeoutSec)*time.Second {
			return fmt.Errorf("no reply within %ds", *timeoutSec)
		}
		time.Sleep(2 * time.Second)
		s, err := gw.GetSession(ctx, sessionID)
		if err != nil {
			continue // transient — keep trying
		}
		if reply := s.LastAssistantText(); reply != "" {
			elapsed := int(time.Since(start).Seconds())
			fmt.Println(output.Header(*cli, elapsed, mode, s.Usage()))
			fmt.Println(reply)
			return nil
		}
	}
}

// pickCompute resolves --on to a compute_id. Empty means "first
// local relay in the user's list."
func pickCompute(ctx context.Context, gw *gateway.Client, query string) (string, error) {
	list, err := gw.ListComputes(ctx)
	if err != nil {
		return "", err
	}
	if query != "" {
		match := gateway.FindCompute(list, query)
		if match == nil {
			return "", fmt.Errorf("no compute matching %q (try `cerver computes`)", query)
		}
		return match.ID, nil
	}
	for _, c := range list {
		if c.Provider == "cerver_local_provider" && c.Status == "ready" {
			return c.ID, nil
		}
	}
	return "", errors.New("no ready local-relay compute (try `cerver computes`)")
}

// resolveBillingMode picks subscription vs api for one CLI on this
// call. Implements the resolution order documented in the skill:
// explicit flag → vendor default. Account-level pref is the future
// step #2 from the design doc — TODO once the gateway endpoint lands.
func resolveBillingMode(cli, flag string) (string, error) {
	switch strings.ToLower(flag) {
	case "subscription", "sub":
		if cli == "grok" {
			return "", errors.New("grok has no subscription mode — use --bill api or remove the flag")
		}
		return "subscription", nil
	case "api":
		return "api", nil
	case "":
		// Default per vendor.
		if cli == "grok" {
			return "api", nil
		}
		return "subscription", nil
	default:
		return "", fmt.Errorf("unknown --bill value %q (use subscription / sub / api)", flag)
	}
}

func apiKeyEnvFor(cli string) string {
	switch cli {
	case "claude":
		return "ANTHROPIC_API_KEY"
	case "codex":
		return "OPENAI_API_KEY"
	case "grok":
		return "XAI_API_KEY"
	}
	return os.Getenv("CLI_API_KEY_NAME") // escape hatch for new vendors
}
