package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Peek prints a one-screen snapshot of a session: id, status, compute,
// CLI tool, last activity, last assistant message (truncated). Designed
// for "is this still running? what did it last say?" — exits fast, no
// streaming.
func Peek(args []string) error {
	fs := flag.NewFlagSet("peek", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: cerver peek <session-id-or-prefix>")
	}
	query := fs.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return errors.New("no cerver credentials — run cerver.ai/install.sh or `cerver login`")
	}
	gw := gateway.New(tok)

	id, err := resolveSessionID(ctx, gw, query)
	if err != nil {
		return err
	}
	s, err := gw.GetSession(ctx, id)
	if err != nil {
		return err
	}

	// Find the most recent assistant text + when it landed.
	lastReply := s.LastAssistantText()
	lastReplyAt := ""
	for i := len(s.Transcript) - 1; i >= 0; i-- {
		e := s.Transcript[i]
		if e.Role == "assistant" && (e.Kind == "text" || e.Kind == "") {
			lastReplyAt = e.At
			break
		}
	}

	cliTool := ""
	if m, ok := s.Metadata["cli_tool"].(string); ok {
		cliTool = m
	}

	fmt.Printf("session  %s\n", s.SessionID)
	fmt.Printf("status   %s\n", statusDot(s.Status))
	fmt.Printf("compute  %s\n", s.ComputeID)
	if cliTool != "" {
		fmt.Printf("cli      %s\n", cliTool)
	}
	if lastReplyAt != "" {
		fmt.Printf("last     %s\n", humanTime(lastReplyAt))
	}
	if lastReply != "" {
		fmt.Printf("reply    %s\n", truncate(lastReply, 200))
	} else {
		fmt.Println("reply    (no assistant reply yet)")
	}
	if u := s.Usage(); u != nil {
		fmt.Printf("usage    %d in / %d out, %d turn(s)\n", u.InputTokens, u.OutputTokens, u.Turns)
	}
	return nil
}

func statusDot(status string) string {
	switch status {
	case "running":
		return "● running"
	case "ready", "idle":
		return "● " + status
	case "failed", "terminated":
		return "○ " + status
	case "completed":
		return "● completed"
	default:
		if status == "" {
			return "—"
		}
		return "● " + status
	}
}
