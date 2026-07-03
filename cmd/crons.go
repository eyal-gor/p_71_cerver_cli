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
)

// Crons is the entry point for `cerver crons ...`. A cron is a scheduled agent
// run attached to a project: on its schedule the gateway fires a normal session
// for the project, so every run inherits the project's spend cap + attribution.
//
//	cerver crons --project SLUG                              list
//	cerver crons create --project SLUG --schedule "0 9 * * *" --prompt "…" [--on COMPUTE]
//	cerver crons run <id> --project SLUG                     fire now (test)
//	cerver crons rm  <id> --project SLUG
func Crons(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return cronsList(rest)
	case "create", "add", "new":
		return cronsCreate(rest)
	case "run", "trigger", "fire":
		return cronsRun(rest)
	case "delete", "rm":
		return cronsDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(cronsHelpText)
		return nil
	default:
		return fmt.Errorf("unknown crons subcommand: %s (try `cerver crons help`)", sub)
	}
}

const cronsHelpText = `cerver crons — scheduled agent runs for a project

usage:
  cerver crons --project SLUG                                list
  cerver crons create --project SLUG --schedule "0 9 * * *" --prompt "…" [--on COMPUTE] [--harness claude]
  cerver crons create --project SLUG --schedule "*/15 * * * *" --agent <agent-id>
  cerver crons create --project SLUG --schedule "* * * * *" --url https://… [--header "x-secret: …"]
  cerver crons run <id> --project SLUG                       fire now (ignores schedule)
  cerver crons rm  <id> --project SLUG

Schedules are 5-field cron expressions in UTC (min hour dom month dow).
--on policy defers compute placement to the project's compute policy (set via
routing-policy compute.consume.providers) — offline-relay fallback included.
Each run is a normal session on the project — its spend cap and attribution
apply automatically, so a runaway cron can't blow the budget.
`

func cronsList(args []string) error {
	fs := flag.NewFlagSet("crons", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("--project SLUG is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	crons, err := gw.ListCrons(ctx, *project)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, crons)
	}
	if len(crons) == 0 {
		fmt.Fprintf(os.Stderr, "no crons yet — create one with `cerver crons create --project %s --schedule \"0 9 * * *\" --prompt ...`\n", *project)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSCHEDULE\tENABLED\tLAST RUN\tNAME\tPROMPT")
	for _, cr := range crons {
		name, prompt, last := "—", "—", "never"
		if cr.Name != nil && *cr.Name != "" {
			name = *cr.Name
		}
		if cr.Prompt != nil && *cr.Prompt != "" {
			prompt = truncate(*cr.Prompt, 40)
		}
		if cr.LastRunAt != nil && *cr.LastRunAt != "" {
			last = *cr.LastRunAt
		}
		fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\t%s\n", cr.ID, cr.Schedule, cr.Enabled, last, name, prompt)
	}
	return tw.Flush()
}

func cronsCreate(args []string) error {
	fs := flag.NewFlagSet("crons create", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	schedule := fs.String("schedule", "", "5-field cron expression, UTC (required)")
	prompt := fs.String("prompt", "", "Task to run")
	agent := fs.String("agent", "", "Saved agent id to run (instead of --prompt)")
	name := fs.String("name", "", "Cron name")
	on := fs.String("on", "", "Compute id to run on, or \"policy\" to let the project's compute policy place it")
	harness := fs.String("harness", "", "CLI: claude | codex | grok")
	model := fs.String("model", "", "Model override")
	webhookURL := fs.String("url", "", "Webhook cron: https URL to call on schedule (instead of an agent run)")
	header := fs.String("header", "", "Webhook header, \"Name: value\" (e.g. \"x-cron-secret: …\")")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *schedule == "" {
		return fmt.Errorf("--project and --schedule are required")
	}
	if *prompt == "" && *agent == "" && *webhookURL == "" {
		return fmt.Errorf("provide --prompt, --agent, or --url (webhook)")
	}
	var headers map[string]string
	if *header != "" {
		parts := strings.SplitN(*header, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("--header must be \"Name: value\"")
		}
		headers = map[string]string{strings.TrimSpace(parts[0]): strings.TrimSpace(parts[1])}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	cr, err := gw.CreateCron(ctx, *project, gateway.CronCreate{
		Schedule: *schedule, Prompt: *prompt, AgentID: *agent, Name: *name,
		ComputeID: *on, Harness: *harness, Model: *model,
		URL: *webhookURL, Headers: headers,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, cr)
	}
	fmt.Printf("✓ cron created  %s  (%s)\n", cr.ID, cr.Schedule)
	return nil
}

func cronsRun(args []string) error {
	fs := flag.NewFlagSet("crons run", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	id := parseIDAndFlags(fs, args)
	if *project == "" || id == "" {
		return fmt.Errorf("usage: cerver crons run <id> --project SLUG")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	sid, err := gw.RunCron(ctx, *project, id)
	if err != nil {
		return err
	}
	fmt.Printf("✓ fired — session %s\n", sid)
	return nil
}

func cronsDelete(args []string) error {
	fs := flag.NewFlagSet("crons delete", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	id := parseIDAndFlags(fs, args)
	if *project == "" || id == "" {
		return fmt.Errorf("usage: cerver crons rm <id> --project SLUG")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteCron(ctx, *project, id); err != nil {
		return err
	}
	fmt.Printf("✓ deleted %s\n", id)
	return nil
}

// parseIDAndFlags accepts both `<id> --flags` and `--flags <id>` — Go's flag
// package stops at the first positional, so a leading id would otherwise
// swallow the flags.
func parseIDAndFlags(fs *flag.FlagSet, args []string) string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		_ = fs.Parse(args[1:])
		return args[0]
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fs.Arg(0)
	}
	return ""
}
