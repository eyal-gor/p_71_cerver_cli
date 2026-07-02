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

// Projects is the entry point for `cerver projects ...`. An "project" in cerver is a
// per-account namespace (slug + name) that groups sessions, API keys, and
// billing under a named integration. A session's `metadata.source` /
// `project_slug` is what the dashboard's Project column shows. The CLI mirrors the
// CRUD surface the /dashboard/projects page exposes.
//
//	cerver projects                                   list (with MTD stats)
//	cerver projects [--json]
//	cerver projects create --name "Kompany" [--slug kompany]
//	cerver projects set-vault --slug kompany --vault ifc_...   (--vault none to clear)
//	cerver projects delete --slug kompany
func Projects(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return projectsList(rest)
	case "create", "add", "new":
		return projectsCreate(rest)
	case "rename", "update":
		return projectsRename(rest)
	case "set-vault", "vault", "bind":
		return projectsSetVault(rest)
	case "delete", "rm", "archive":
		return projectsDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(projectsHelpText)
		return nil
	default:
		return fmt.Errorf("unknown projects subcommand: %s (try `cerver projects help`)", sub)
	}
}

const projectsHelpText = `cerver projects — manage your projects (per-account namespaces for sessions/keys/billing)

usage:
  cerver projects                                   list (with this month's stats)
  cerver projects [--json]
  cerver projects create --name "Kompany" [--slug kompany]
  cerver projects rename --slug cron [--name "Kompany"] [--new-slug kompany]
  cerver projects set-vault --slug kompany --vault ifc_...   (--vault none to clear)
  cerver projects delete --slug kompany

Renaming the slug does NOT relabel already-created sessions (their Project column
is the source slug stamped at creation). A project's slug is what shows in the
dashboard's Project column. Bind a default
vault to a project with set-vault, or attach environments + repos with:
  cerver envs create --project SLUG --slug prod
`

func projectsList(args []string) error {
	fs := flag.NewFlagSet("projects", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	projects, err := gw.ListProjects(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, projects)
	}
	if len(projects) == 0 {
		fmt.Fprintln(os.Stderr, "no projects yet — create one with `cerver projects create --name ...`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tNAME\tKEYS\tSESSIONS(MTD)\tSPEND(MTD)\tVAULT\tID")
	for _, a := range projects {
		vault := "—"
		if a.InfisicalConfigLabel != nil && *a.InfisicalConfigLabel != "" {
			vault = *a.InfisicalConfigLabel
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t$%.4f\t%s\t%s\n",
			a.Slug, a.Name, a.APIKeyCount, a.SessionCountMTD, a.TotalUsdMTD, vault, a.ID)
	}
	return tw.Flush()
}

func projectsCreate(args []string) error {
	fs := flag.NewFlagSet("projects create", flag.ContinueOnError)
	name := fs.String("name", "", "Display name e.g. 'Kompany' (required)")
	slug := fs.String("slug", "", "URL-safe slug (defaults to the normalized name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	project, err := gw.CreateProject(ctx, gateway.ProjectCreate{Name: *name, Slug: *slug})
	if err != nil {
		return err
	}
	fmt.Printf("created project %s (slug %s, %s)\n", project.Name, project.Slug, project.ID)
	return nil
}

func projectsRename(args []string) error {
	fs := flag.NewFlagSet("projects rename", flag.ContinueOnError)
	slug := fs.String("slug", "", "Current project slug (required)")
	name := fs.String("name", "", "New display name")
	newSlug := fs.String("new-slug", "", "New slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("--slug is required (the project to rename)")
	}
	if *name == "" && *newSlug == "" {
		return fmt.Errorf("pass --name and/or --new-slug")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	project, err := gw.RenameProject(ctx, *slug, gateway.ProjectRename{Name: *name, Slug: *newSlug})
	if err != nil {
		return err
	}
	fmt.Printf("renamed → name %q, slug %q (%s)\n", project.Name, project.Slug, project.ID)
	return nil
}

func projectsSetVault(args []string) error {
	fs := flag.NewFlagSet("projects set-vault", flag.ContinueOnError)
	slug := fs.String("slug", "", "Project slug (required)")
	vault := fs.String("vault", "", "Vault id ifc_... to bind, or 'none' to clear (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" || *vault == "" {
		return fmt.Errorf("--slug and --vault are required (use --vault none to clear)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	var cfg *string
	if *vault != "none" {
		cfg = vault
	}
	if err := gw.SetProjectInfisicalConfig(ctx, *slug, cfg); err != nil {
		return err
	}
	if cfg == nil {
		fmt.Printf("cleared vault on project %s\n", *slug)
	} else {
		fmt.Printf("bound vault %s to project %s\n", *vault, *slug)
	}
	return nil
}

func projectsDelete(args []string) error {
	fs := flag.NewFlagSet("projects delete", flag.ContinueOnError)
	slug := fs.String("slug", "", "Project slug (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("--slug is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.ArchiveProject(ctx, *slug); err != nil {
		return err
	}
	fmt.Printf("archived project %s\n", *slug)
	return nil
}
