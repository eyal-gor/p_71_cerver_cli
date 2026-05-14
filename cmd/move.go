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

// Move changes the compute of an existing session. The new compute
// inherits the transcript; the old agent is terminated. Same
// session_id is preserved on cerver's side.
func Move(args []string) error {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: cerver move <session-id> <compute-name-or-id>")
	}
	sessionQuery := fs.Arg(0)
	computeQuery := fs.Arg(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return errors.New("no cerver credentials — run cerver.ai/install.sh or `cerver login`")
	}
	gw := gateway.New(tok)

	sessionID, err := resolveSessionID(ctx, gw, sessionQuery)
	if err != nil {
		return err
	}

	// Resolve the target compute by name OR id, the same way `run --on`
	// does — keeps the user from having to paste comp_ uuids.
	computes, err := gw.ListComputes(ctx)
	if err != nil {
		return err
	}
	target := gateway.FindCompute(computes, computeQuery)
	if target == nil {
		return fmt.Errorf("no compute matching %q (try `cerver computes`)", computeQuery)
	}

	if err := gw.ChangeCompute(ctx, sessionID, target.ID); err != nil {
		return err
	}
	fmt.Printf("moved %s → %s (%s)\n", shortID(sessionID), target.Label, target.ID)
	return nil
}
