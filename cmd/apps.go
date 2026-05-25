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

// Apps is the entry point for `cerver apps ...`. An "app" in cerver is a
// per-account namespace (slug + name) that groups sessions, API keys, and
// billing under a named integration. A session's `metadata.source` /
// `app_slug` is what the dashboard's App column shows. The CLI mirrors the
// CRUD surface the /dashboard/apps page exposes.
//
//	cerver apps                                   list (with MTD stats)
//	cerver apps [--json]
//	cerver apps create --name "Kompany" [--slug kompany]
//	cerver apps set-vault --slug kompany --vault ifc_...   (--vault none to clear)
//	cerver apps delete --slug kompany
func Apps(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return appsList(rest)
	case "create", "add", "new":
		return appsCreate(rest)
	case "set-vault", "vault", "bind":
		return appsSetVault(rest)
	case "delete", "rm", "archive":
		return appsDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(appsHelpText)
		return nil
	default:
		return fmt.Errorf("unknown apps subcommand: %s (try `cerver apps help`)", sub)
	}
}

const appsHelpText = `cerver apps — manage your apps (per-account namespaces for sessions/keys/billing)

usage:
  cerver apps                                   list (with this month's stats)
  cerver apps [--json]
  cerver apps create --name "Kompany" [--slug kompany]
  cerver apps set-vault --slug kompany --vault ifc_...   (--vault none to clear)
  cerver apps delete --slug kompany

An app's slug is what shows in the dashboard's App column. Bind a default
vault to an app with set-vault, or attach environments + repos with:
  cerver envs create --app SLUG --slug prod
`

func appsList(args []string) error {
	fs := flag.NewFlagSet("apps", flag.ContinueOnError)
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
	apps, err := gw.ListApps(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, apps)
	}
	if len(apps) == 0 {
		fmt.Fprintln(os.Stderr, "no apps yet — create one with `cerver apps create --name ...`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tNAME\tKEYS\tSESSIONS(MTD)\tSPEND(MTD)\tVAULT\tID")
	for _, a := range apps {
		vault := "—"
		if a.InfisicalConfigLabel != nil && *a.InfisicalConfigLabel != "" {
			vault = *a.InfisicalConfigLabel
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t$%.4f\t%s\t%s\n",
			a.Slug, a.Name, a.APIKeyCount, a.SessionCountMTD, a.TotalUsdMTD, vault, a.ID)
	}
	return tw.Flush()
}

func appsCreate(args []string) error {
	fs := flag.NewFlagSet("apps create", flag.ContinueOnError)
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
	app, err := gw.CreateApp(ctx, gateway.AppCreate{Name: *name, Slug: *slug})
	if err != nil {
		return err
	}
	fmt.Printf("created app %s (slug %s, %s)\n", app.Name, app.Slug, app.ID)
	return nil
}

func appsSetVault(args []string) error {
	fs := flag.NewFlagSet("apps set-vault", flag.ContinueOnError)
	slug := fs.String("slug", "", "App slug (required)")
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
	if err := gw.SetAppInfisicalConfig(ctx, *slug, cfg); err != nil {
		return err
	}
	if cfg == nil {
		fmt.Printf("cleared vault on app %s\n", *slug)
	} else {
		fmt.Printf("bound vault %s to app %s\n", *vault, *slug)
	}
	return nil
}

func appsDelete(args []string) error {
	fs := flag.NewFlagSet("apps delete", flag.ContinueOnError)
	slug := fs.String("slug", "", "App slug (required)")
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
	if err := gw.ArchiveApp(ctx, *slug); err != nil {
		return err
	}
	fmt.Printf("archived app %s\n", *slug)
	return nil
}
