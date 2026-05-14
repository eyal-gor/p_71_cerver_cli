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

// Sessions lists recent sessions for the authenticated account.
//
// Default view is a tabwriter table. `--json` for scripting. `--limit
// N` to bump past the 20 default.
func Sessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "Max sessions to fetch")
	jsonOut := fs.Bool("json", false, "Emit raw JSON instead of a table")
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
		return fmt.Errorf("no cerver credentials — run cerver.ai/install.sh or `cerver login`")
	}
	gw := gateway.New(tok)
	list, err := gw.ListSessions(ctx, *limit)
	if err != nil {
		return err
	}

	if *jsonOut {
		return jsonEncode(os.Stdout, list)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tNAME\tCLI\tSTATUS\tUPDATED")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID(s.SessionID), truncate(s.SessionName, 24),
			s.CliTool(), s.Status, humanTime(s.UpdatedAt))
	}
	return w.Flush()
}

// shortID renders the first 8 chars of a UUID so the table fits in
// 80 columns. The full id is still accepted by all other verbs.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

// humanTime renders an RFC3339 string as a relative "5m ago" / "2h ago".
// Falls back to the original string if parsing fails.
func humanTime(s string) string {
	if s == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// resolveSessionID accepts either a full uuid or a short prefix (e.g.
// "f5e27f6c") and returns the full session id by listing recent
// sessions and prefix-matching. Lets users paste the short id from
// the `sessions` table directly into peek/show/move/kill.
func resolveSessionID(ctx context.Context, gw *gateway.Client, query string) (string, error) {
	query = strings.TrimSpace(query)
	if len(query) >= 32 {
		return query, nil // full uuid
	}
	if query == "" {
		return "", fmt.Errorf("session id required")
	}
	list, err := gw.ListSessions(ctx, 50)
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if strings.HasPrefix(s.SessionID, query) {
			return s.SessionID, nil
		}
	}
	return "", fmt.Errorf("no recent session starts with %q — run `cerver sessions` to see ids", query)
}
