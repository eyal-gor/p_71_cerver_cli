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

// Envs is the entry point for `cerver envs ...`. Mirrors what the
// /dashboard/environments page lets you do via the gateway, so users
// can CRUD environments + their repo bindings without touching the UI.
//
// Verb shape:
//
//	cerver envs                              list every env across every project
//	cerver envs [--project slug] [--json]        list (filtered)
//	cerver envs create --project slug --slug s [--name N] [--default] [--infisical ifc_]
//	cerver envs update --project slug --env slug [--name N] [--default] [--infisical ifc_|none]
//	cerver envs delete --project slug --env slug
//	cerver envs repos  --project slug --env slug [--json]
//	cerver envs repos add --project slug --env slug --url URL [--ref R] [--primary]
//	cerver envs repos rm  --project slug --env slug --repo-id rep_...
//
// The default verb (no subcommand) is `list`.
func Envs(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return envsList(rest)
	case "create", "new", "add":
		return envsCreate(rest)
	case "update", "edit", "set":
		return envsUpdate(rest)
	case "delete", "rm", "archive":
		return envsDelete(rest)
	case "repos":
		return envsRepos(rest)
	case "help", "-h", "--help":
		fmt.Print(envsHelpText)
		return nil
	default:
		return fmt.Errorf("unknown envs subcommand: %s (try `cerver envs help`)", sub)
	}
}

const envsHelpText = `cerver envs — manage project environments

usage:
  cerver envs                              list every env across every project
  cerver envs [--project slug] [--json]
  cerver envs create --project slug --slug s [--name N] [--default] [--infisical ifc_]
  cerver envs update --project slug --env slug [--name N] [--default] [--infisical ifc_|none]
  cerver envs delete --project slug --env slug
  cerver envs repos  --project slug --env slug [--json]
  cerver envs repos add --project slug --env slug --url URL [--ref R] [--primary]
  cerver envs repos rm  --project slug --env slug --repo-id rep_...
`

func envsList(args []string) error {
	fs := flag.NewFlagSet("envs list", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (omit to list every project's envs)")
	jsonOut := fs.Bool("json", false, "Emit raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}

	type row struct {
		ProjectSlug string                `json:"project_slug"`
		ProjectName string                `json:"project_name"`
		Env     gateway.Environment   `json:"env"`
	}
	var rows []row

	if *project != "" {
		envs, err := gw.ListEnvironments(ctx, *project)
		if err != nil {
			return err
		}
		for _, e := range envs {
			rows = append(rows, row{ProjectSlug: e.ProjectSlug, ProjectName: e.ProjectName, Env: e})
		}
	} else {
		// Fan out across every project on the account so the bare `cerver
		// envs` view matches the /dashboard/environments page.
		projects, err := gw.ListProjects(ctx)
		if err != nil {
			return err
		}
		for _, a := range projects {
			envs, err := gw.ListEnvironments(ctx, a.Slug)
			if err != nil {
				// A single project failing shouldn't blank the whole table;
				// surface a warning row and keep going.
				fmt.Fprintf(os.Stderr, "warn: %s envs: %v\n", a.Slug, err)
				continue
			}
			for _, e := range envs {
				rows = append(rows, row{ProjectSlug: a.Slug, ProjectName: a.Name, Env: e})
			}
		}
	}

	if *jsonOut {
		return encodeJSON(os.Stdout, rows)
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no environments yet — create one with `cerver envs create --project SLUG --slug prod`")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENV\tAPP\tDEFAULT\tINFISICAL\tREPOS\tID")
	for _, r := range rows {
		def := ""
		if r.Env.IsDefault {
			def = "★"
		}
		inf := ""
		if r.Env.InfisicalConfigLabel != nil && *r.Env.InfisicalConfigLabel != "" {
			inf = *r.Env.InfisicalConfigLabel
		} else if r.Env.ProjectInfisicalConfigLabel != nil && *r.Env.ProjectInfisicalConfigLabel != "" {
			inf = *r.Env.ProjectInfisicalConfigLabel + " (project)"
		} else {
			inf = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			r.Env.Slug, r.ProjectSlug, def, inf, r.Env.RepoCount, r.Env.ID)
	}
	return tw.Flush()
}

func envsCreate(args []string) error {
	fs := flag.NewFlagSet("envs create", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	slug := fs.String("slug", "", "Env slug e.g. prod / staging (required)")
	name := fs.String("name", "", "Env display name (defaults to slug)")
	def := fs.Bool("default", false, "Mark as the project's default env")
	infi := fs.String("infisical", "", "Infisical config id to override the project's default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *slug == "" {
		return fmt.Errorf("--project and --slug are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}

	body := gateway.EnvCreate{Slug: *slug, Name: *name, IsDefault: *def}
	if *infi != "" {
		body.InfisicalConfigID = infi
	}
	env, err := gw.CreateEnvironment(ctx, *project, body)
	if err != nil {
		return err
	}
	fmt.Printf("created %s/%s (id=%s, default=%v)\n", env.ProjectSlug, env.Slug, env.ID, env.IsDefault)
	return nil
}

func envsUpdate(args []string) error {
	fs := flag.NewFlagSet("envs update", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	name := fs.String("name", "", "New display name")
	def := fs.String("default", "", "set 'true' to mark as default, 'false' to unset")
	infi := fs.String("infisical", "", "Infisical config id, or 'none' to clear")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *envSlug == "" {
		return fmt.Errorf("--project and --env are required")
	}

	body := gateway.EnvUpdate{}
	if *name != "" {
		body.Name = name
	}
	if *def != "" {
		v := *def == "true" || *def == "1" || *def == "yes"
		body.IsDefault = &v
	}
	if *infi != "" {
		if *infi == "none" || *infi == "null" {
			empty := ""
			body.InfisicalConfigID = &empty
		} else {
			body.InfisicalConfigID = infi
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	env, err := gw.UpdateEnvironment(ctx, *project, *envSlug, body)
	if err != nil {
		return err
	}
	fmt.Printf("updated %s/%s (default=%v)\n", env.ProjectSlug, env.Slug, env.IsDefault)
	return nil
}

func envsDelete(args []string) error {
	fs := flag.NewFlagSet("envs delete", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *envSlug == "" {
		return fmt.Errorf("--project and --env are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteEnvironment(ctx, *project, *envSlug); err != nil {
		return err
	}
	fmt.Printf("archived %s/%s\n", *project, *envSlug)
	return nil
}

func envsRepos(args []string) error {
	// repos has its own micro-dispatch
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return envsReposList(rest)
	case "add", "new":
		return envsReposAdd(rest)
	case "rm", "remove", "delete":
		return envsReposRm(rest)
	default:
		return fmt.Errorf("unknown envs repos subcommand: %s", sub)
	}
}

func envsReposList(args []string) error {
	fs := flag.NewFlagSet("envs repos list", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	jsonOut := fs.Bool("json", false, "Emit raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *envSlug == "" {
		return fmt.Errorf("--project and --env are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	repos, err := gw.ListEnvRepos(ctx, *project, *envSlug)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, repos)
	}
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "no repos yet — add one with `cerver envs repos add --project ... --env ... --url ...`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRIMARY\tREF\tURL")
	for _, r := range repos {
		prim := ""
		if r.IsPrimary {
			prim = "★"
		}
		ref := ""
		if r.RepoRef != nil {
			ref = *r.RepoRef
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, prim, ref, r.RepoURL)
	}
	return tw.Flush()
}

func envsReposAdd(args []string) error {
	fs := flag.NewFlagSet("envs repos add", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	repoURL := fs.String("url", "", "Repo URL e.g. https://github.com/o/r.git (required)")
	ref := fs.String("ref", "", "Optional git ref")
	primary := fs.Bool("primary", false, "Mark as the env's primary repo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *envSlug == "" || *repoURL == "" {
		return fmt.Errorf("--project, --env and --url are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	rep, err := gw.CreateEnvRepo(ctx, *project, *envSlug, gateway.EnvRepoCreate{
		RepoURL: *repoURL, RepoRef: *ref, IsPrimary: *primary,
	})
	if err != nil {
		return err
	}
	fmt.Printf("added %s (primary=%v) to %s/%s\n", rep.ID, rep.IsPrimary, *project, *envSlug)
	return nil
}

func envsReposRm(args []string) error {
	fs := flag.NewFlagSet("envs repos rm", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	repoID := fs.String("repo-id", "", "Repo id e.g. rep_... (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *envSlug == "" || *repoID == "" {
		return fmt.Errorf("--project, --env and --repo-id are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteEnvRepo(ctx, *project, *envSlug, *repoID); err != nil {
		return err
	}
	fmt.Printf("removed %s from %s/%s\n", *repoID, *project, *envSlug)
	return nil
}

// authedClient is the same boilerplate every cmd needs: load the token
// from the user's Infisical, fail loud if missing, return a Client.
// Local to envs so a botched copy/paste here can't break older verbs.
func authedClient(ctx context.Context) (*gateway.Client, error) {
	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, fmt.Errorf("no cerver credentials found — run cerver.ai/install.sh first")
	}
	return gateway.New(tok), nil
}
