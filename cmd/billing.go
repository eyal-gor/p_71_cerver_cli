package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Billing prints the caller's month-to-date cerver bill: service fees
// (cerver's margin), database fees (pass-through of Neon costs), plus
// a per-session breakdown of the top spenders.
//
//   cerver billing                       # this month so far
//   cerver billing --since 2026-05-01    # custom window
//   cerver billing --json                # raw JSON for scripting
//
// Read-only — the actual charging isn't wired yet (phase 3, Stripe).
func Billing(args []string) error {
	fs := flag.NewFlagSet("billing", flag.ContinueOnError)
	since := fs.String("since", "", "Start of window (ISO 8601). Default: start of current UTC month.")
	until := fs.String("until", "", "End of window (ISO 8601). Default: now.")
	jsonOut := fs.Bool("json", false, "Emit raw JSON instead of a table.")
	limit := fs.Int("limit", 10, "Max sessions to show in the breakdown.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return errors.New("no cerver credentials — run `curl -fsSL https://cerver.ai/install.sh | bash` first")
	}
	gw := gateway.New(tok)

	summary, err := gw.GetBilling(ctx, *since, *until)
	if err != nil {
		return err
	}

	if *jsonOut {
		return jsonEncode(os.Stdout, summary)
	}

	t := summary.Totals
	fmt.Printf("Cerver billing  %s → %s\n", short(summary.Period.Start), short(summary.Period.End))
	fmt.Println()
	fmt.Printf("  service fee       $%.4f   (%.0f sessions × $0.002)\n", t.ServiceUSD, summary.Counts.Sessions)
	fmt.Printf("  database egress   $%.4f   (%s)\n", t.DBEgressUSD, humanBytes(summary.Counts.BytesOut))
	fmt.Printf("  database compute  $%.4f   (%s)\n", t.DBComputeUSD, humanMillis(summary.Counts.ComputeMS))
	fmt.Printf("  ─────────────────────────\n")
	fmt.Printf("  total             $%.4f\n", t.TotalUSD)
	fmt.Println()
	fmt.Println("  (observe-only mode — not yet billed)")
	fmt.Println()

	if len(summary.BySession) == 0 {
		return nil
	}
	n := *limit
	if n > len(summary.BySession) {
		n = len(summary.BySession)
	}
	fmt.Printf("Top %d sessions by cost:\n", n)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tUSD\tBYTES OUT\tCOMPUTE\tLAST SEEN")
	for i := 0; i < n; i++ {
		s := summary.BySession[i]
		fmt.Fprintf(w, "%s\t$%.4f\t%s\t%s\t%s\n",
			shortID(s.SessionID),
			s.TotalUSD,
			humanBytes(s.BytesOut),
			humanMillis(s.ComputeMS),
			humanTime(s.LastSeen),
		)
	}
	return w.Flush()
}

// short renders an ISO 8601 timestamp as a compact date string for the
// "since → until" header line. Falls back to the input on parse error.
func short(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func humanBytes(b float64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%.0f B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KB", b/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", b/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GB", b/(1024*1024*1024))
	}
}

func humanMillis(ms float64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%.0f ms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1f s", ms/1000)
	default:
		return fmt.Sprintf("%.1f min", ms/60000)
	}
}
