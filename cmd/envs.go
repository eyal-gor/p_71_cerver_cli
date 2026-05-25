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
//	cerver envs                              list every env across every app
//	cerver envs [--app slug] [--json]        list (filtered)
//	cerver envs create --app slug --slug s [--name N] [--default] [--infisical ifc_]
//	cerver envs update --app slug --env slug [--name N] [--default] [--infisical ifc_|none]
//	cerver envs delete --app slug --env slug
//	cerver envs repos  --app slug --env slug [--json]
//	cerver envs repos add --app slug --env slug --url URL [--ref R] [--primary]
//	cerver envs repos rm  --app slug --env slug --repo-id rep_...
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

const envsHelpText = `cerver envs — manage app environments

usage:
  cerver envs                              list every env across every app
  cerver envs [--app slug] [--json]
  cerver envs create --app slug --slug s [--name N] [--default] [--infisical ifc_]
  cerver envs update --app slug --env slug [--name N] [--default] [--infisical ifc_|none]
  cerver envs delete --app slug --env slug
  cerver envs repos  --app slug --env slug [--json]
  cerver envs repos add --app slug --env slug --url URL [--ref R] [--primary]
  cerver envs repos rm  --app slug --env slug --repo-id rep_...
`

func envsList(args []string) error {
	fs := flag.NewFlagSet("envs list", flag.ContinueOnError)
	app := fs.String("app", "", "App slug (omit to list every app's envs)")
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
		AppSlug string                `json:"app_slug"`
		AppName string                `json:"app_name"`
		Env     gateway.Environment   `json:"env"`
	}
	var rows []row

	if *app != "" {
		envs, err := gw.ListEnvironments(ctx, *app)
		if err != nil {
			return err
		}
		for _, e := range envs {
			rows = append(rows, row{AppSlug: e.AppSlug, AppName: e.AppName, Env: e})
		}
	} else {
		// Fan out across every app on the account so the bare `cerver
		// envs` view matches the /dashboard/environments page.
		apps, err := gw.ListApps(ctx)
		if err != nil {
			return err
		}
		for _, a := range apps {
			envs, err := gw.ListEnvironments(ctx, a.Slug)
			if err != nil {
				// A single app failing shouldn't blank the whole table;
				// surface a warning row and keep going.
				fmt.Fprintf(os.Stderr, "warn: %s envs: %v\n", a.Slug, err)
				continue
			}
			for _, e := range envs {
				rows = append(rows, row{AppSlug: a.Slug, AppName: a.Name, Env: e})
			}
		}
	}

	if *jsonOut {
		return encodeJSON(os.Stdout, rows)
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no environments yet — create one with `cerver envs create --app SLUG --slug prod`")
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
		} else if r.Env.AppInfisicalConfigLabel != nil && *r.Env.AppInfisicalConfigLabel != "" {
			inf = *r.Env.AppInfisicalConfigLabel + " (app)"
		} else {
			inf = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			r.Env.Slug, r.AppSlug, def, inf, r.Env.RepoCount, r.Env.ID)
	}
	return tw.Flush()
}

func envsCreate(args []string) error {
	fs := flag.NewFlagSet("envs create", flag.ContinueOnError)
	app := fs.String("app", "", "App slug (required)")
	slug := fs.String("slug", "", "Env slug e.g. prod / staging (required)")
	name := fs.String("name", "", "Env display name (defaults to slug)")
	def := fs.Bool("default", false, "Mark as the app's default env")
	infi := fs.String("infisical", "", "Infisical config id to override the app's default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" || *slug == "" {
		return fmt.Errorf("--app and --slug are required")
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
	env, err := gw.CreateEnvironment(ctx, *app, body)
	if err != nil {
		return err
	}
	fmt.Printf("created %s/%s (id=%s, default=%v)\n", env.AppSlug, env.Slug, env.ID, env.IsDefault)
	return nil
}

func envsUpdate(args []string) error {
	fs := flag.NewFlagSet("envs update", flag.ContinueOnError)
	app := fs.String("app", "", "App slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	name := fs.String("name", "", "New display name")
	def := fs.String("default", "", "set 'true' to mark as default, 'false' to unset")
	infi := fs.String("infisical", "", "Infisical config id, or 'none' to clear")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" || *envSlug == "" {
		return fmt.Errorf("--app and --env are required")
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
	env, err := gw.UpdateEnvironment(ctx, *app, *envSlug, body)
	if err != nil {
		return err
	}
	fmt.Printf("updated %s/%s (default=%v)\n", env.AppSlug, env.Slug, env.IsDefault)
	return nil
}

func envsDelete(args []string) error {
	fs := flag.NewFlagSet("envs delete", flag.ContinueOnError)
	app := fs.String("app", "", "App slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" || *envSlug == "" {
		return fmt.Errorf("--app and --env are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteEnvironment(ctx, *app, *envSlug); err != nil {
		return err
	}
	fmt.Printf("archived %s/%s\n", *app, *envSlug)
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
	app := fs.String("app", "", "App slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	jsonOut := fs.Bool("json", false, "Emit raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" || *envSlug == "" {
		return fmt.Errorf("--app and --env are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	repos, err := gw.ListEnvRepos(ctx, *app, *envSlug)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, repos)
	}
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "no repos yet — add one with `cerver envs repos add --app ... --env ... --url ...`")
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
	app := fs.String("app", "", "App slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	repoURL := fs.String("url", "", "Repo URL e.g. https://github.com/o/r.git (required)")
	ref := fs.String("ref", "", "Optional git ref")
	primary := fs.Bool("primary", false, "Mark as the env's primary repo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" || *envSlug == "" || *repoURL == "" {
		return fmt.Errorf("--app, --env and --url are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	rep, err := gw.CreateEnvRepo(ctx, *app, *envSlug, gateway.EnvRepoCreate{
		RepoURL: *repoURL, RepoRef: *ref, IsPrimary: *primary,
	})
	if err != nil {
		return err
	}
	fmt.Printf("added %s (primary=%v) to %s/%s\n", rep.ID, rep.IsPrimary, *app, *envSlug)
	return nil
}

func envsReposRm(args []string) error {
	fs := flag.NewFlagSet("envs repos rm", flag.ContinueOnError)
	app := fs.String("app", "", "App slug (required)")
	envSlug := fs.String("env", "", "Env slug (required)")
	repoID := fs.String("repo-id", "", "Repo id e.g. rep_... (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *app == "" || *envSlug == "" || *repoID == "" {
		return fmt.Errorf("--app, --env and --repo-id are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteEnvRepo(ctx, *app, *envSlug, *repoID); err != nil {
		return err
	}
	fmt.Printf("removed %s from %s/%s\n", *repoID, *app, *envSlug)
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
