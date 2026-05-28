package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Suggestions is the entry point for
//
//	cerver suggestions [list|new|upstream]
//
// Default verb is list — `cerver suggestions` prints the user's recent
// suggestions in a table. `new` files a private note (per-account
// visible only). `upstream` is the explicit-opt-in equivalent: same
// shape, but flags the row so the cerver maintainers can aggregate
// it across accounts. Agents must only invoke `upstream` when the
// user actually asked to share the note.
func Suggestions(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "new", "file", "add":
			return suggestionsNew(args[1:])
		case "upstream", "share", "u":
			return suggestionsUpstream(args[1:])
		case "list", "ls":
			return suggestionsList(args[1:])
		}
	}
	return suggestionsList(args)
}

func suggestionsList(args []string) error {
	fs := flag.NewFlagSet("suggestions list", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "Max suggestions to fetch")
	status := fs.String("status", "", "Filter by status (open|resolved|...)")
	surface := fs.String("surface", "", "Filter by surface (skill|cli|relay)")
	cliTool := fs.String("cli", "", "Filter by CLI tool (claude|codex|grok)")
	jsonOut := fs.Bool("json", false, "Emit raw JSON")
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
		return errors.New("no cerver credentials — run cerver.ai/install.sh or `cerver login`")
	}
	gw := gateway.New(tok)

	list, err := gw.ListSuggestions(ctx, gateway.SuggestionFilters{
		Limit:   *limit,
		Status:  *status,
		Surface: *surface,
		CliTool: *cliTool,
	})
	if err != nil {
		return err
	}

	if *jsonOut {
		return jsonEncode(os.Stdout, list)
	}

	if len(list) == 0 {
		fmt.Println("No suggestions yet.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSURFACE\tCLI\tSTATUS\tWHEN\tSUMMARY")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(s.ID),
			gateway.Deref(s.Surface),
			gateway.Deref(s.CliTool),
			s.Status,
			humanTime(s.CreatedAt),
			truncate(s.Summary, 60),
		)
	}
	return w.Flush()
}

func suggestionsNew(args []string) error {
	fs := flag.NewFlagSet("suggestions new", flag.ContinueOnError)
	surface := fs.String("surface", "cli", "Where the friction came from: skill|cli|relay")
	cliTool := fs.String("cli", "", "Which CLI surfaced the issue (claude|codex|grok)")
	sessionID := fs.String("session", "", "Session id that triggered this, if any")
	detail := fs.String("detail", "", "Longer freeform description")
	if err := fs.Parse(args); err != nil {
		return err
	}
	summary := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if summary == "" {
		return errors.New(`usage: cerver suggestions new [--surface skill|cli|relay] [--cli claude|codex|grok] [--session <id>] [--detail "..."] "one-line summary"`)
	}

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

	s, err := gw.FileSuggestion(ctx, gateway.CreateSuggestion{
		Summary:   summary,
		Detail:    *detail,
		Surface:   *surface,
		CliTool:   *cliTool,
		SessionID: *sessionID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("filed %s\n", s.ID)
	return nil
}

// suggestionsUpstream is the explicit opt-in share path: same body
// as `suggestions new`, but POSTs to /v2/suggestions/upstream so the
// row is flagged for the cerver maintainers to see across accounts.
// Use this only when the user actually asked to share their note.
func suggestionsUpstream(args []string) error {
	fs := flag.NewFlagSet("suggestions upstream", flag.ContinueOnError)
	surface := fs.String("surface", "cli", "Where the friction came from: skill|cli|relay")
	cliTool := fs.String("cli", "", "Which CLI surfaced the issue (claude|codex|grok)")
	sessionID := fs.String("session", "", "Session id that triggered this, if any")
	detail := fs.String("detail", "", "Longer freeform description")
	if err := fs.Parse(args); err != nil {
		return err
	}
	summary := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if summary == "" {
		return errors.New(`usage: cerver suggestions upstream [--surface skill|cli|relay] [--cli claude|codex|grok] [--session <id>] [--detail "..."] "one-line summary"`)
	}

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

	s, err := gw.FileUpstreamSuggestion(ctx, gateway.CreateSuggestion{
		Summary:   summary,
		Detail:    *detail,
		Surface:   *surface,
		CliTool:   *cliTool,
		SessionID: *sessionID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("filed %s  (shared upstream with cerver maintainers)\n", s.ID)
	return nil
}
