package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Insights asks the gateway to run the "read between the lines" agent
// over the account's recent sessions and prints a structured report:
// what users keep asking, where they get stuck, what features the
// product should ship next.
//
//	cerver insights                       # default: last 30 sessions across all projects
//	cerver insights --project SLUG            # filter to one project
//	cerver insights --limit 50            # consider more sessions
//	cerver insights --json                # raw JSON for piping
func Insights(args []string) error {
	fs := flag.NewFlagSet("insights", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug to scope the analysis (omit for everything)")
	limit := fs.Int("limit", 0, "Max sessions to consider (server caps at 100)")
	days := fs.Int("days", 0, "Look-back window in days")
	jsonOut := fs.Bool("json", false, "Emit raw JSON instead of a formatted report")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// LLM analysis can take a few seconds; give it room.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	report, err := gw.GenerateInsights(ctx, *project, *days, *limit)
	if err != nil {
		return err
	}

	if *jsonOut {
		return encodeJSON(os.Stdout, report)
	}

	// Human-readable report. Tabular formatting felt wrong here; this
	// is documentary, not scannable.
	if report.AnalyzedSessions == 0 {
		fmt.Println(report.Summary)
		return nil
	}
	fmt.Println()
	scope := "all projects"
	if *project != "" {
		scope = "project " + *project
	}
	fmt.Printf("INSIGHTS · %s · %d sessions seen / %d analyzed\n", scope, report.SessionCount, report.AnalyzedSessions)
	fmt.Println(strings.Repeat("─", 72))
	fmt.Println()
	fmt.Println("Summary")
	fmt.Println("  " + report.Summary)
	if len(report.TopAsks) > 0 {
		fmt.Println()
		fmt.Println("Top asks")
		for _, s := range report.TopAsks {
			fmt.Println("  · " + s)
		}
	}
	if len(report.StuckPatterns) > 0 {
		fmt.Println()
		fmt.Println("Where users get stuck")
		for _, s := range report.StuckPatterns {
			fmt.Println("  · " + s)
		}
	}
	if len(report.SuggestedFeatures) > 0 {
		fmt.Println()
		fmt.Println("Features to consider shipping")
		for _, s := range report.SuggestedFeatures {
			fmt.Println("  · " + s)
		}
	}
	fmt.Println()
	if report.ParseError {
		fmt.Println("(note: the model returned non-JSON; output above is best-effort.)")
	}
	return nil
}
