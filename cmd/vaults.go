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

// Vaults is the entry point for `cerver vaults ...`. A "vault" in cerver
// is one Infisical-project connection (stored as a row in
// cerver_account_infisical_configs, AES-GCM encrypted). The CLI mirrors
// the same CRUD surface the /dashboard/vault page exposes.
//
//	cerver vaults                                       list
//	cerver vaults [--json]
//	cerver vaults add --label N --client-id ID --client-secret SEC --project-id PID [--env prod] [--site-url URL] [--default]
//	cerver vaults rename --id ifc_... --label NEW
//	cerver vaults set-default --id ifc_...
//	cerver vaults verify --id ifc_...
//	cerver vaults delete --id ifc_...
func Vaults(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return vaultsList(rest)
	case "add", "create", "new":
		return vaultsAdd(rest)
	case "rename", "label":
		return vaultsRename(rest)
	case "set-default", "default":
		return vaultsSetDefault(rest)
	case "verify", "check":
		return vaultsVerify(rest)
	case "delete", "rm":
		return vaultsDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(vaultsHelpText)
		return nil
	default:
		return fmt.Errorf("unknown vaults subcommand: %s (try `cerver vaults help`)", sub)
	}
}

const vaultsHelpText = `cerver vaults — manage your Infisical vaults (the per-account secret connections)

usage:
  cerver vaults                                       list
  cerver vaults [--json]
  cerver vaults add --label N --client-id ID --client-secret SEC --project-id PID   (infisical)
  cerver vaults add --provider doppler --label N --token dp.st…
  cerver vaults add --provider cerver  --label N --secret ANTHROPIC_API_KEY=sk-… [--secret X=Y]
                    [--env prod] [--site-url URL] [--default]
  cerver vaults rename --id ifc_... --label NEW
  cerver vaults set-default --id ifc_...
  cerver vaults verify --id ifc_...
  cerver vaults delete --id ifc_...

A "vault" is one Infisical project that cerver can read/write on your
behalf. Bind a vault to a project or environment with:
  cerver envs update --project SLUG --env ENV --infisical ifc_...
`

func vaultsList(args []string) error {
	fs := flag.NewFlagSet("vaults list", flag.ContinueOnError)
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
	cfgs, err := gw.ListInfisicalConfigs(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, cfgs)
	}
	if len(cfgs) == 0 {
		fmt.Fprintln(os.Stderr, "no vaults yet — add one with `cerver vaults add --label ... --client-id ... --client-secret ... --project-id ...`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tDEFAULT\tPROJECT\tENV\tVERIFIED\tID")
	for _, c := range cfgs {
		def := ""
		if c.IsDefault {
			def = "★"
		}
		ver := "—"
		if c.LastVerifyError != nil && *c.LastVerifyError != "" {
			ver = "✗ " + *c.LastVerifyError
		} else if c.LastVerifiedAt != nil && *c.LastVerifiedAt != "" {
			ver = "✓"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Label, def, c.ProjectID, c.Environment, ver, c.ID)
	}
	return tw.Flush()
}

func vaultsAdd(args []string) error {
	fs := flag.NewFlagSet("vaults add", flag.ContinueOnError)
	provider := fs.String("provider", "infisical", "Vault provider: infisical | doppler | cerver")
	label := fs.String("label", "", "Display name e.g. 'kompany' (required)")
	clientID := fs.String("client-id", "", "Infisical UA client id")
	clientSecret := fs.String("client-secret", "", "Infisical UA client secret (not echoed back)")
	projectID := fs.String("project-id", "", "Infisical project (workspace) id")
	env := fs.String("env", "prod", "Infisical environment slug")
	siteURL := fs.String("site-url", "", "Override the Infisical API base URL (self-hosted)")
	token := fs.String("token", "", "Doppler service token (dp.st…)")
	var secretKVs multiFlag
	fs.Var(&secretKVs, "secret", "cerver vault secret NAME=VALUE (repeatable)")
	def := fs.Bool("default", false, "Make this the account default (unsets the previous default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *label == "" {
		return fmt.Errorf("--label is required")
	}
	req := gateway.InfisicalConfigCreate{
		Provider: *provider, Label: *label, Environment: *env, IsDefault: *def,
	}
	switch *provider {
	case "infisical":
		if *clientID == "" || *clientSecret == "" || *projectID == "" {
			return fmt.Errorf("infisical vaults need --client-id, --client-secret and --project-id")
		}
		req.ClientID, req.ClientSecret, req.ProjectID, req.SiteURL = *clientID, *clientSecret, *projectID, *siteURL
	case "doppler":
		if *token == "" {
			return fmt.Errorf("doppler vaults need --token (a Doppler service token)")
		}
		req.Token = *token
	case "cerver":
		if len(secretKVs) == 0 {
			return fmt.Errorf("cerver vaults need at least one --secret NAME=VALUE")
		}
		req.Secrets = map[string]string{}
		for _, kv := range secretKVs {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 || parts[0] == "" {
				return fmt.Errorf("--secret must be NAME=VALUE, got %q", kv)
			}
			req.Secrets[parts[0]] = parts[1]
		}
	default:
		return fmt.Errorf("unknown --provider %q (infisical | doppler | cerver)", *provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	id, err := gw.CreateInfisicalConfig(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("added %s vault %s (%s)\n", *provider, *label, id)
	return nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func vaultsRename(args []string) error {
	fs := flag.NewFlagSet("vaults rename", flag.ContinueOnError)
	id := fs.String("id", "", "Vault id (required)")
	label := fs.String("label", "", "New label (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *label == "" {
		return fmt.Errorf("--id and --label are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.UpdateInfisicalConfig(ctx, *id, gateway.InfisicalConfigUpdate{Label: label}); err != nil {
		return err
	}
	fmt.Printf("renamed %s → %s\n", *id, *label)
	return nil
}

func vaultsSetDefault(args []string) error {
	fs := flag.NewFlagSet("vaults set-default", flag.ContinueOnError)
	id := fs.String("id", "", "Vault id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	yes := true
	if err := gw.UpdateInfisicalConfig(ctx, *id, gateway.InfisicalConfigUpdate{IsDefault: &yes}); err != nil {
		return err
	}
	fmt.Printf("%s is now the default vault\n", *id)
	return nil
}

func vaultsVerify(args []string) error {
	fs := flag.NewFlagSet("vaults verify", flag.ContinueOnError)
	id := fs.String("id", "", "Vault id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.VerifyInfisicalConfig(ctx, *id); err != nil {
		return err
	}
	fmt.Printf("verified %s\n", *id)
	return nil
}

func vaultsDelete(args []string) error {
	fs := flag.NewFlagSet("vaults delete", flag.ContinueOnError)
	id := fs.String("id", "", "Vault id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	if err := gw.DeleteInfisicalConfig(ctx, *id); err != nil {
		return err
	}
	fmt.Printf("deleted vault %s\n", *id)
	return nil
}
