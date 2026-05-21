package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Computes lists what the account can run on. Two tables:
//
//   1. INSTANCES — actual computes that exist right now. Your laptop
//      relays, any warm shared-provider sandbox-relays. Each has a
//      stable compute_id you can target with `--on <comp_id>`.
//
//   2. PROVIDERS — request handles ("give me a fresh sandbox of this
//      kind"). Always present; never refer to a specific machine.
//      Use these with `--on provider_<name>` when you want a new one,
//      not when you want to reuse an existing instance.
//
// JSON output keeps the flat one-list shape for scripting compat.
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

	// Split the flat API response into the two semantic groups.
	// The "provider_" id-prefix is the gateway's convention for
	// request handles (see v2/sessions/service.ts's `provider_`
	// branch); everything else is a concrete instance.
	var instances, providers []gateway.Compute
	for _, c := range list {
		if strings.HasPrefix(c.ID, "provider_") {
			providers = append(providers, c)
		} else {
			instances = append(instances, c)
		}
	}

	fmt.Println("INSTANCES")
	if len(instances) == 0 {
		fmt.Println("  (none — start a local relay or request a sandbox-relay via `cerver run --on provider_…`)")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  ID\tLABEL\tPROVIDER\tSTATUS")
		for _, c := range instances {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.ID, c.Label, c.Provider, c.Status)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	fmt.Println()
	fmt.Println("PROVIDERS")
	if len(providers) == 0 {
		fmt.Println("  (none)")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  ID\tLABEL\tPROVIDER\tSTATUS")
		for _, c := range providers {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.ID, c.Label, c.Provider, c.Status)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// encodeJSON is in its own helper so we can extend it (pretty print,
// optionally include extra fields) without touching the caller.
func encodeJSON(w *os.File, v any) error {
	// Use the std encoder for now. If we ever want consistent ordering
	// we can swap to a custom emit.
	return jsonEncode(w, v)
}
