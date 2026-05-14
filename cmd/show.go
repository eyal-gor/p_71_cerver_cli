package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Show prints the full transcript for a session.
//
// --follow keeps polling and prints new entries as they land (Ctrl+C
// to exit). Useful for tailing a long-running compare or a cron.
// --tail N limits to the last N entries instead of the whole transcript.
func Show(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "Keep polling and stream new entries")
	tailN := fs.Int("tail", 0, "Only show the last N entries (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: cerver show <session-id-or-prefix> [--follow] [--tail N]")
	}
	query := fs.Arg(0)

	ctx := context.Background()
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

	// Initial dump.
	s, err := gw.GetSession(ctx, id)
	if err != nil {
		return err
	}
	entries := s.Transcript
	if *tailN > 0 && len(entries) > *tailN {
		entries = entries[len(entries)-*tailN:]
	}
	for _, e := range entries {
		printEntry(e)
	}
	if !*follow {
		return nil
	}

	// Follow mode: poll every 2s, print new entries by index. Use the
	// transcript length as our cursor — cerver only appends, so the
	// length is a stable monotonic cursor across polls.
	cursor := len(s.Transcript)
	for {
		time.Sleep(2 * time.Second)
		s, err := gw.GetSession(ctx, id)
		if err != nil {
			continue // transient
		}
		for i := cursor; i < len(s.Transcript); i++ {
			printEntry(s.Transcript[i])
		}
		cursor = len(s.Transcript)
		if s.Status != "running" && cursor > 0 {
			// One more tick to flush, then exit cleanly.
			time.Sleep(2 * time.Second)
			s, _ := gw.GetSession(ctx, id)
			for i := cursor; i < len(s.Transcript); i++ {
				printEntry(s.Transcript[i])
			}
			return nil
		}
	}
}

func printEntry(e gateway.TranscriptEntry) {
	tstr := ""
	if e.At != "" {
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			tstr = t.Format("15:04:05")
		}
	}
	kind := e.Kind
	if kind == "" {
		kind = "text"
	}
	prefix := fmt.Sprintf("[%s] %-10s", tstr, e.Role+"/"+kind)
	// Indent multi-line content under the prefix so it reads as a block.
	for i, line := range strings.Split(e.Content, "\n") {
		if i == 0 {
			fmt.Println(prefix + " " + line)
		} else {
			fmt.Println(strings.Repeat(" ", len(prefix)+1) + line)
		}
	}
}
