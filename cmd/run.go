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
//
// With --resume <sid> the prompt is sent as a follow-up to an
// existing session instead of creating a new one. The session keeps
// its same transcript, compute, and CLI — those are owned by the
// session, not by this invocation — so --cli / --on / --model are
// rejected when paired with --resume.
func Run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cli := fs.String("cli", "claude", "CLI to run: claude | codex | grok")
	on := fs.String("on", "", "Compute name or id to run on (default: first local relay)")
	bill := fs.String("bill", "", "Billing mode: subscription | api (alias: sub | api)")
	model := fs.String("model", "", "Model override (e.g. sonnet, opus, gpt-5-codex). Empty = CLI's local default.")
	timeoutSec := fs.Int("timeout", 180, "Max seconds to wait for the reply")
	resume := fs.String("resume", "", "Session id (or short prefix) to resume — sends prompt as a follow-up to an existing session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return errors.New("usage: cerver run [flags] \"your prompt\"")
	}

	// Detect which flags the user passed (vs. carry their defaults).
	// Needed because we want to allow `--cli` etc. for fresh runs but
	// reject them when paired with --resume — the existing session
	// owns those choices, accepting them would imply we'd swap CLI
	// mid-session, which we don't do yet.
	passedFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passedFlags[f.Name] = true })

	if *resume != "" {
		for _, name := range []string{"cli", "on", "model"} {
			if passedFlags[name] {
				return fmt.Errorf("--%s can't be combined with --resume (the existing session owns its %s — switch is a future feature)", name, name)
			}
		}
	}

	mode, err := resolveBillingMode(*cli, *bill)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*timeoutSec)*time.Second+10*time.Second)
	defer cancel()

	cerverTok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if cerverTok == "" {
		return errors.New("no cerver credentials found — run `curl https://cerver.ai/install.sh | bash` first")
	}

	gw := gateway.New(cerverTok)

	var sessionID string
	cursor := 0
	if *resume != "" {
		// Resume path — no session create, no compute pick. POST input
		// to the existing session; the gateway uses native CLI resume
		// (claude --resume, codex exec resume) under the hood via the
		// recovery hints recorded in session metadata.
		sid, err := resolveSessionID(ctx, gw, *resume)
		if err != nil {
			return err
		}
		sessionID = sid
		// Seed cursor BEFORE SendInput so WaitForReplyFromCursor doesn't
		// race on the previous turn's assistant text. Codex's audit
		// flagged this specifically — without the seed, the next
		// /v2/sessions GET returns the old reply and we exit too early.
		probe, err := gw.GetSession(ctx, sessionID)
		if err == nil {
			if probe.TranscriptTotal > 0 {
				cursor = probe.TranscriptTotal
			} else {
				cursor = len(probe.Transcript)
			}
		}
	} else {
		// Fresh-session path. Pick compute, create session.
		computeID, err := pickCompute(ctx, gw, *on)
		if err != nil {
			return err
		}
		// `complete_on_exit: true` used to live here as the "this is a
		// one-shot, free the relay slot fast" hint. It also (unintentionally)
		// broke resume: the gateway took a different code path that
		// didn't record native CLI session ids in metadata, and the
		// relay deleted its agent record. Dropped — sessions are now
		// genuinely resumable by default; the relay can still recycle
		// its in-process slot on CLI exit (separate concern).
		metadata := map[string]any{
			"cli_tool": *cli,
			// Identifies the originating app/client to the gateway so
			// the dashboard's "App" column reads "cerver-cli" instead
			// of blank. Custom apps (e.g. invest-watch's chat panel)
			// set their own metadata.source value.
			"source": "cerver-cli",
		}
		if *model != "" {
			metadata["cli_model"] = *model
		}
		sid, err := gw.CreateSession(ctx, gateway.SessionCreate{
			SessionType: "coding",
			Compute:     map[string]any{"compute_id": computeID},
			Task:        prompt,
			Workload:    "coding",
			// Use the first line of the prompt (truncated) as the
			// session label so the relay TUI's "Cerver sessions" table
			// shows what each run was actually about instead of a
			// row of identical "cli-run" labels. Trim at the first
			// newline so multi-line prompts don't smear across the
			// column.
			SessionName: shortPromptLabel(prompt, 48),
			Metadata:    metadata,
		})
		if err != nil {
			return err
		}
		sessionID = sid
	}

	if err := gw.SendInput(ctx, sessionID, prompt); err != nil {
		return err
	}

	start := time.Now()
	s, err := gw.WaitForReplyFromCursor(ctx, sessionID, cursor,
		time.Duration(*timeoutSec)*time.Second,
		8*time.Second)
	if err != nil {
		return err
	}
	elapsed := int(time.Since(start).Seconds())
	fmt.Println(output.Header(*cli, elapsed, mode, s.Usage()))
	fmt.Println(s.LastAssistantText())
	return nil
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

// shortPromptLabel returns a one-line, length-capped form of the
// user's prompt suitable for a session-name column in lists. Strips
// leading whitespace, takes the first line only, and adds an ellipsis
// when truncating mid-word so the label doesn't look like a fragment.
func shortPromptLabel(prompt string, maxLen int) string {
	s := strings.TrimSpace(prompt)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "cli-run"
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
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
