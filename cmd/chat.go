package cmd

import (
	"bufio"
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

// Chat is a multi-turn conversation over one cerver session. Use it
// when you want the agent to keep context across messages — long
// investigations, iterative reviews, follow-up questions.
//
//   cerver chat                       # new session, defaults to claude
//   cerver chat <sid>                 # resume an existing session
//   cerver chat --cli codex --model gpt-5-codex
//
// Each input line POSTs to /v2/sessions/<sid>/input, then we wait for
// the agent's reply via the cursor-based poll loop. Ctrl+D or :exit
// ends the session — the session id is printed on exit so you can
// resume it later with `cerver chat <sid>`.
func Chat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	cli := fs.String("cli", "claude", "CLI to use for a fresh session: claude | codex | grok")
	on := fs.String("on", "", "Compute name or id (default: first local relay)")
	bill := fs.String("bill", "", "Billing mode: subscription | api (alias: sub)")
	model := fs.String("model", "", "Model override for a fresh session (e.g. sonnet, opus, gpt-5-codex)")
	turnTimeoutSec := fs.Int("turn-timeout", 240, "Max seconds to wait for one turn's reply")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mode, err := resolveBillingMode(*cli, *bill)
	if err != nil {
		return err
	}

	ctx := context.Background()
	cerverTok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if cerverTok == "" {
		return errors.New("no cerver credentials — run `curl -fsSL https://cerver.ai/install.sh | bash` first")
	}

	// Vendor API keys only when --bill api.
	envInject := map[string]string{}
	if mode == "api" {
		icfg, err := infisical.LoadConfig()
		if err != nil {
			return fmt.Errorf("--bill api needs Infisical for the vendor key: %w", err)
		}
		inf := infisical.New(icfg)
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

	// Either resume an existing session or create a new one. We resume
	// when the user passes a session id (or short prefix) as a positional
	// arg — exactly like `cerver show <sid>`.
	var sessionID string
	if fs.NArg() > 0 {
		sid, err := resolveSessionID(ctx, gw, fs.Arg(0))
		if err != nil {
			return err
		}
		sessionID = sid
		fmt.Fprintf(os.Stderr, "resuming %s\n", shortID(sessionID))
	} else {
		computeID, err := pickCompute(ctx, gw, *on)
		if err != nil {
			return err
		}
		metadata := map[string]any{"cli_tool": *cli}
		if *model != "" {
			metadata["cli_model"] = *model
		}
		if len(envInject) > 0 {
			metadata["env"] = envInject
		}
		// `passive: true` defers CLI spawn until the first POST /input —
		// otherwise the relay tries to spawn the agent at create-time
		// with no prompt and the CLI exits immediately.
		metadata["passive"] = true
		sid, err := gw.CreateSession(ctx, gateway.SessionCreate{
			SessionType: "coding",
			Compute:     map[string]any{"compute_id": computeID},
			Task:        "",
			Workload:    "coding",
			SessionName: "cli-chat",
			Metadata:    metadata,
		})
		if err != nil {
			return err
		}
		sessionID = sid
		fmt.Fprintf(os.Stderr, "session %s · type :exit or Ctrl+D to end\n", shortID(sessionID))
	}

	turnTimeout := time.Duration(*turnTimeoutSec) * time.Second
	scanner := bufio.NewScanner(os.Stdin)
	// 1MB line buffer so long pasted prompts don't get truncated. Default
	// bufio.Scanner cap is 64K which is too tight for multi-paragraph asks.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	isTTY := isStdinTTY()

	// Track the transcript cursor across turns. When resuming an existing
	// session, seed from the server so the first WaitForReply doesn't
	// return the previous turn's reply as if it were a new one. For fresh
	// sessions, cursor=0.
	cursor := 0
	if fs.NArg() > 0 {
		probe, err := gw.GetSession(ctx, sessionID)
		if err == nil {
			if probe.TranscriptTotal > 0 {
				cursor = probe.TranscriptTotal
			} else {
				cursor = len(probe.Transcript)
			}
		}
	}

	for {
		if isTTY {
			fmt.Print("you> ")
		}
		if !scanner.Scan() {
			break // EOF / Ctrl+D
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == ":exit" || line == ":quit" {
			break
		}

		turnCtx, cancel := context.WithTimeout(ctx, turnTimeout+15*time.Second)
		if err := gw.SendInput(turnCtx, sessionID, line); err != nil {
			cancel()
			fmt.Fprintf(os.Stderr, "input failed: %v\n", err)
			continue
		}
		// SendInput appends the user message to the transcript (one entry).
		// Advance the cursor past it so WaitForReply only counts the
		// agent's reply entries as "new activity" for this turn.
		cursor++
		start := time.Now()
		s, err := gw.WaitForReplyFromCursor(turnCtx, sessionID, cursor, turnTimeout, 8*time.Second)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "wait failed: %v\n", err)
			continue
		}
		// Advance cursor to the new end-of-transcript so the next turn
		// starts from a clean baseline. TranscriptTotal is set by
		// WaitForReplyFromCursor from the server's count.
		if s.TranscriptTotal > cursor {
			cursor = s.TranscriptTotal
		}
		elapsed := int(time.Since(start).Seconds())
		fmt.Println(output.Header(*cli, elapsed, mode, s.Usage()))
		fmt.Println(s.LastAssistantText())
		fmt.Println()
	}

	fmt.Fprintf(os.Stderr, "session %s ended · resume with `cerver chat %s`\n", shortID(sessionID), sessionID)
	return nil
}

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
