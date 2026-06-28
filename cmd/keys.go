package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// Keys is the entry point for `cerver keys ...`. Every API key in cerver is
// bound to exactly one app — the key IS the app's credential. Sessions, env,
// and permissions follow that app boundary. Creating a key with --app
// get-or-creates the app; omit --app and the key falls to your "default" app.
//
//	cerver keys                                  list (masked, with app)
//	cerver keys create --app kompany [--label "prod server"]
//	cerver keys create --app widget --publishable      (pk_ for client HTML)
//	cerver keys delete --prefix ck_1a2b
func Keys(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return keysList(rest)
	case "create", "add", "new":
		return keysCreate(rest)
	case "delete", "rm", "revoke":
		return keysDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(keysHelpText)
		return nil
	default:
		return fmt.Errorf("unknown keys subcommand: %s (try `cerver keys help`)", sub)
	}
}

const keysHelpText = `cerver keys — manage app-scoped API keys

Every key is bound to one app. The app is the boundary: a key's sessions, env,
and permissions all belong to its app. --app get-or-creates the app on the fly;
omit it and the key lands in your "default" app.

usage:
  cerver keys                                  list your keys (masked) + their app
  cerver keys create --app kompany [--label "prod server"]
  cerver keys create --app widget --publishable      mint a pk_ key for client HTML
  cerver keys delete --prefix ck_1a2b          revoke a key by prefix

A secret key (ck_) is shown ONCE at creation — copy it then. Publishable keys
(pk_) are spend-capped and safe to ship in a browser; they require --app.
`

func keysList(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
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
	keys, err := gw.ListKeys(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, keys)
	}
	if len(keys) == 0 {
		fmt.Fprintln(os.Stderr, "no keys yet — create one with `cerver keys create --app ...`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tLABEL\tAPP\tLAST USED")
	for _, k := range keys {
		app := "—"
		if k.AppSlug != nil && *k.AppSlug != "" {
			app = *k.AppSlug
		}
		last := "never"
		if k.LastUsedAt != nil && *k.LastUsedAt != "" {
			last = *k.LastUsedAt
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.KeyMasked, k.Label, app, last)
	}
	return tw.Flush()
}

func keysCreate(args []string) error {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	app := fs.String("app", "", "App slug to bind the key to (auto-created; defaults to your \"default\" app)")
	label := fs.String("label", "", "Human label for the key")
	publishable := fs.Bool("publishable", false, "Mint a spend-capped pk_ key for client HTML (requires --app)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *publishable && *app == "" {
		return fmt.Errorf("--publishable keys must be bound to an app — pass --app SLUG")
	}
	kind := ""
	if *publishable {
		kind = "publishable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	created, err := gw.CreateKey(ctx, gateway.KeyCreate{Label: *label, AppSlug: *app, Kind: kind})
	if err != nil {
		return err
	}
	appSlug := "default"
	if created.AppSlug != nil && *created.AppSlug != "" {
		appSlug = *created.AppSlug
	}
	fmt.Printf("created %s key for app %q\n", created.Kind, appSlug)
	fmt.Printf("  %s\n", created.Key)
	fmt.Fprintln(os.Stderr, "↑ copy this now — a secret key is shown only once.")
	return nil
}

func keysDelete(args []string) error {
	fs := flag.NewFlagSet("keys delete", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "Key prefix to revoke, e.g. ck_1a2b (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *prefix == "" {
		return fmt.Errorf("--prefix is required (the first chars of the key to revoke)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteKey(ctx, *prefix); err != nil {
		return err
	}
	fmt.Printf("revoked key %s\n", *prefix)
	return nil
}
