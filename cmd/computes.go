package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Computes lists every compute the account has access to. Tabwriter
// gives us a clean aligned table without pulling in a TUI lib.
func Computes(args []string) error {
	fs := flag.NewFlagSet("computes", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Emit raw JSON instead of a table (for scripting)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return fmt.Errorf("no cerver credentials found — run cerver.ai/install.sh first")
	}
	gw := gateway.New(tok)

	list, err := gw.ListComputes(ctx)
	if err != nil {
		return err
	}

	if *jsonOut {
		// JSON output: re-encode the slice as-is so callers can pipe.
		return encodeJSON(os.Stdout, list)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLABEL\tPROVIDER\tSTATUS")
	for _, c := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.ID, c.Label, c.Provider, c.Status)
	}
	return w.Flush()
}

// encodeJSON is in its own helper so we can extend it (pretty print,
// optionally include extra fields) without touching the caller.
func encodeJSON(w *os.File, v any) error {
	// Use the std encoder for now. If we ever want consistent ordering
	// we can swap to a custom emit.
	return jsonEncode(w, v)
}
