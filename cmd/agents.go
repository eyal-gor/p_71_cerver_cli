package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// Agents is the entry point for `cerver agents ...`. A saved agent is a
// reusable definition — an AGENTS.md (instructions dropped into the session
// workspace) plus a config map (preferred harness/model, workload). Apply one
// to a run with `cerver run --agent <slug> "..."`; the gateway injects the
// AGENTS.md and applies the config defaults (explicit --cli/--model still win).
//
//	cerver agents                                   list
//	cerver agents [--json]
//	cerver agents show <id|slug>
//	cerver agents new --name "Reviewer" [--md-file AGENTS.md] [--harness claude] [--model opus] [--workload coding] [--slug reviewer] [--app SLUG] [--config-file cfg.json]
//	cerver agents edit <id|slug> [--name ...] [--md-file ...] [--harness ...] [--model ...] [--workload ...] [--config-file ...]
//	cerver agents rm <id|slug>
//	cerver agents pull <id|slug> [--dir .]          write AGENTS.md + agent.json locally
//	cerver agents push [<id|slug>] [--dir .]        create/update from local AGENTS.md (+ agent.json)
func Agents(args []string) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return agentsList(rest)
	case "show", "get", "cat":
		return agentsShow(rest)
	case "new", "create", "add":
		return agentsCreate(rest)
	case "edit", "update":
		return agentsEdit(rest)
	case "rm", "delete", "remove":
		return agentsDelete(rest)
	case "pull", "export":
		return agentsPull(rest)
	case "push", "import", "sync":
		return agentsPush(rest)
	case "help", "-h", "--help":
		fmt.Print(agentsHelpText)
		return nil
	default:
		return fmt.Errorf("unknown agents subcommand: %s (try `cerver agents help`)", sub)
	}
}

const agentsHelpText = `cerver agents — save reusable agent definitions (AGENTS.md + config)

An agent bundles an AGENTS.md (instructions dropped into the session workspace)
with config defaults (harness/model/workload). Apply one to a run:

  cerver run --agent <slug> "do the thing"

usage:
  cerver agents                                   list your agents
  cerver agents [--json]
  cerver agents show <id|slug>                    print config + AGENTS.md
  cerver agents new --name "Reviewer" [flags]     create
  cerver agents edit <id|slug> [flags]            update (only passed fields)
  cerver agents rm <id|slug>                      delete
  cerver agents pull <id|slug> [--dir .]          write AGENTS.md + agent.json
  cerver agents push [<id|slug>] [--dir .]        create/update from local files

flags (new / edit):
  --name N            display name
  --slug S            url-safe slug (defaults to normalized name)
  --md-file FILE      read AGENTS.md from FILE  (or --md "inline text")
  --harness H         preferred CLI: claude | codex | grok
  --model M           preferred model (sonnet, opus, gpt-5-codex, …)
  --workload W        workload hint (coding, …)
  --app SLUG          scope the agent to this app (agents are app-scoped by default)
  --global            make the agent account-wide instead — the explicit opt-out
                      of app scoping (new: must pass --app or --global)
  --config-file FILE  raw JSON config (overlaid by the flags above)

pull writes <dir>/AGENTS.md and <dir>/agent.json — edit them in your editor,
then 'push' the same dir to sync. push without an id/slug creates a new agent
(or updates the one named in agent.json).
`

// --- helpers ---------------------------------------------------------------

// agentFlags is the shared create/edit flag block.
type agentFlags struct {
	name, slug, md, mdFile   *string
	harness, model, workload *string
	app, configFile          *string
}

func registerAgentFlags(fs *flag.FlagSet) agentFlags {
	return agentFlags{
		name:       fs.String("name", "", "Display name"),
		slug:       fs.String("slug", "", "URL-safe slug (defaults to the normalized name)"),
		md:         fs.String("md", "", "AGENTS.md content, inline"),
		mdFile:     fs.String("md-file", "", "Read AGENTS.md content from this file"),
		harness:    fs.String("harness", "", "Preferred CLI: claude | codex | grok"),
		model:      fs.String("model", "", "Preferred model (sonnet, opus, gpt-5-codex, …)"),
		workload:   fs.String("workload", "", "Workload hint (e.g. coding)"),
		app:        fs.String("app", "", "Scope the agent to one app slug (default: account-wide)"),
		configFile: fs.String("config-file", "", "Raw JSON config file (overlaid by --harness/--model/--workload)"),
	}
}

// resolveAgentsMD returns the AGENTS.md text from --md-file or --md (file wins),
// and whether either was provided.
func (af agentFlags) resolveAgentsMD() (string, bool, error) {
	if *af.mdFile != "" {
		b, err := os.ReadFile(*af.mdFile)
		if err != nil {
			return "", false, fmt.Errorf("reading --md-file: %w", err)
		}
		return string(b), true, nil
	}
	if *af.md != "" {
		return *af.md, true, nil
	}
	return "", false, nil
}

// buildConfig merges --config-file (if any) with the individual flags. Returns
// the config map and whether anything was set.
func (af agentFlags) buildConfig() (map[string]any, bool, error) {
	cfg := map[string]any{}
	set := false
	if *af.configFile != "" {
		b, err := os.ReadFile(*af.configFile)
		if err != nil {
			return nil, false, fmt.Errorf("reading --config-file: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, false, fmt.Errorf("--config-file is not valid JSON: %w", err)
		}
		set = true
	}
	if *af.harness != "" {
		cfg["harness"] = *af.harness
		set = true
	}
	if *af.model != "" {
		cfg["model"] = *af.model
		set = true
	}
	if *af.workload != "" {
		cfg["workload"] = *af.workload
		set = true
	}
	return cfg, set, nil
}

// agentValueFlags are the flags that consume the following token as their
// value. Needed by splitAgentRef so it doesn't mistake a flag's value for the
// positional id/slug.
var agentValueFlags = map[string]bool{
	"name": true, "slug": true, "md": true, "md-file": true, "harness": true,
	"model": true, "workload": true, "app": true, "config-file": true, "dir": true,
}

// splitAgentRef pulls the positional id/slug out of a subcommand's args and
// returns it plus the remaining (flag-only) args, which can then be handed to
// flag.Parse. This sidesteps Go's flag package stopping at the first
// non-flag token — so `agents edit <ref> --model opus` parses --model.
func splitAgentRef(args []string) (ref string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			// "--flag value" form: the next token is the value, not the ref.
			if !strings.Contains(a, "=") && agentValueFlags[name] && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if ref == "" {
			ref = a
		} else {
			rest = append(rest, a)
		}
	}
	return ref, rest
}

func cfgStr(cfg map[string]any, key string) string {
	if cfg == nil {
		return ""
	}
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}

// --- subcommands -----------------------------------------------------------

func agentsList(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Output JSON")
	app := fs.String("app", "", "Filter to one app's agents + globals (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	agents, err := gw.ListAgents(ctx, *app)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, agents)
	}
	if len(agents) == 0 {
		fmt.Fprintln(os.Stderr, "no agents yet — create one with `cerver agents new --name ...`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tNAME\tSCOPE\tHARNESS\tMODEL\tUPDATED\tID")
	for _, a := range agents {
		h := cfgStr(a.Config, "harness")
		if h == "" {
			h = "—"
		}
		m := cfgStr(a.Config, "model")
		if m == "" {
			m = "—"
		}
		scope := a.AppSlug
		if scope == "" {
			scope = "global"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Slug, a.Name, scope, h, m, humanTime(a.UpdatedAt), a.ID)
	}
	return tw.Flush()
}

func agentsShow(args []string) error {
	ref, rest := splitAgentRef(args)
	fs := flag.NewFlagSet("agents show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("usage: cerver agents show <id|slug>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	a, err := gw.ResolveAgent(ctx, ref)
	if err != nil {
		return err
	}
	if *jsonOut {
		return encodeJSON(os.Stdout, a)
	}
	fmt.Printf("%s  (slug %s, %s)\n", a.Name, a.Slug, a.ID)
	if a.AppSlug != "" {
		fmt.Printf("scope:  app %s\n", a.AppSlug)
	} else {
		fmt.Printf("scope:  global\n")
	}
	if len(a.Config) > 0 {
		cfg, _ := json.MarshalIndent(a.Config, "", "  ")
		fmt.Printf("config: %s\n", string(cfg))
	}
	fmt.Println("--- AGENTS.md ---")
	if strings.TrimSpace(a.AgentsMD) == "" {
		fmt.Println("(empty)")
	} else {
		fmt.Println(a.AgentsMD)
	}
	return nil
}

func agentsCreate(args []string) error {
	fs := flag.NewFlagSet("agents new", flag.ContinueOnError)
	af := registerAgentFlags(fs)
	// Agents are app-scoped by default — a global agent must be asked for
	// explicitly. This prevents an agent silently leaking account-wide just
	// because the creator forgot --app.
	global := fs.Bool("global", false, "Make the agent account-wide (visible in every app). Default is app-scoped via --app.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *af.name == "" {
		return fmt.Errorf("--name is required")
	}
	if *af.app == "" && !*global {
		return fmt.Errorf("agents are app-scoped by default — pass --app <slug> to scope it, or --global for an account-wide agent")
	}
	if *af.app != "" && *global {
		return fmt.Errorf("--app and --global are mutually exclusive")
	}
	md, _, err := af.resolveAgentsMD()
	if err != nil {
		return err
	}
	cfg, _, err := af.buildConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	a, err := gw.CreateAgent(ctx, gateway.AgentWrite{
		Name: *af.name, Slug: *af.slug, AgentsMD: md, Config: cfg, AppSlug: *af.app, Global: *global,
	})
	if err != nil {
		return err
	}
	fmt.Printf("created agent %s (slug %s, %s)\n", a.Name, a.Slug, a.ID)
	fmt.Printf("apply it:  cerver run --agent %s \"...\"\n", a.Slug)
	return nil
}

func agentsEdit(args []string) error {
	ref, rest := splitAgentRef(args)
	fs := flag.NewFlagSet("agents edit", flag.ContinueOnError)
	af := registerAgentFlags(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("usage: cerver agents edit <id|slug> [flags]")
	}
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	cur, err := gw.ResolveAgent(ctx, ref)
	if err != nil {
		return err
	}

	body := gateway.AgentWrite{Name: *af.name, Slug: *af.slug, AppSlug: *af.app}
	if md, ok, err := af.resolveAgentsMD(); err != nil {
		return err
	} else if ok {
		body.AgentsMD = md
	}
	// Merge config onto the current one so an edit only touches passed keys.
	if passed["harness"] || passed["model"] || passed["workload"] || passed["config-file"] {
		merged := map[string]any{}
		for k, v := range cur.Config {
			merged[k] = v
		}
		delta, _, err := af.buildConfig()
		if err != nil {
			return err
		}
		for k, v := range delta {
			merged[k] = v
		}
		body.Config = merged
	}
	a, err := gw.UpdateAgent(ctx, cur.ID, body)
	if err != nil {
		return err
	}
	fmt.Printf("updated agent %s (slug %s, %s)\n", a.Name, a.Slug, a.ID)
	return nil
}

func agentsDelete(args []string) error {
	ref, rest := splitAgentRef(args)
	fs := flag.NewFlagSet("agents rm", flag.ContinueOnError)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("usage: cerver agents rm <id|slug>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	a, err := gw.ResolveAgent(ctx, ref)
	if err != nil {
		return err
	}
	if err := gw.DeleteAgent(ctx, a.ID); err != nil {
		return err
	}
	fmt.Printf("deleted agent %s (%s)\n", a.Slug, a.ID)
	return nil
}

// agentFile is the sidecar written by `pull` and read by `push` — everything
// except the AGENTS.md body, which lives in AGENTS.md next to it.
type agentFile struct {
	ID      string         `json:"id,omitempty"`
	Name    string         `json:"name"`
	Slug    string         `json:"slug"`
	Config  map[string]any `json:"config,omitempty"`
	AppSlug string         `json:"app_slug,omitempty"`
}

func agentsPull(args []string) error {
	ref, rest := splitAgentRef(args)
	fs := flag.NewFlagSet("agents pull", flag.ContinueOnError)
	dir := fs.String("dir", ".", "Directory to write AGENTS.md + agent.json into")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if ref == "" {
		return fmt.Errorf("usage: cerver agents pull <id|slug> [--dir .]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}
	a, err := gw.ResolveAgent(ctx, ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	mdPath := filepath.Join(*dir, "AGENTS.md")
	if err := os.WriteFile(mdPath, []byte(a.AgentsMD), 0o644); err != nil {
		return err
	}
	side := agentFile{ID: a.ID, Name: a.Name, Slug: a.Slug, Config: a.Config, AppSlug: a.AppSlug}
	sb, _ := json.MarshalIndent(side, "", "  ")
	jsonPath := filepath.Join(*dir, "agent.json")
	if err := os.WriteFile(jsonPath, append(sb, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("pulled %s → %s, %s\n", a.Slug, mdPath, jsonPath)
	return nil
}

func agentsPush(args []string) error {
	posRef, rest := splitAgentRef(args)
	fs := flag.NewFlagSet("agents push", flag.ContinueOnError)
	dir := fs.String("dir", ".", "Directory holding AGENTS.md (+ optional agent.json)")
	name := fs.String("name", "", "Override name (else from agent.json / dir name)")
	slug := fs.String("slug", "", "Override slug")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	mdPath := filepath.Join(*dir, "AGENTS.md")
	mdBytes, err := os.ReadFile(mdPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", mdPath, err)
	}
	// Sidecar is optional — absent means a fresh create from flags/dir name.
	var side agentFile
	if sb, err := os.ReadFile(filepath.Join(*dir, "agent.json")); err == nil {
		if err := json.Unmarshal(sb, &side); err != nil {
			return fmt.Errorf("agent.json is not valid JSON: %w", err)
		}
	}
	if *name != "" {
		side.Name = *name
	}
	if *slug != "" {
		side.Slug = *slug
	}
	if side.Name == "" {
		abs, _ := filepath.Abs(*dir)
		side.Name = filepath.Base(abs)
	}

	// An explicit positional id/slug, or the sidecar's id, selects update.
	ref := side.ID
	if posRef != "" {
		ref = posRef
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := authedClient(ctx)
	if err != nil {
		return err
	}

	body := gateway.AgentWrite{
		Name: side.Name, Slug: side.Slug, AgentsMD: string(mdBytes),
		Config: side.Config, AppSlug: side.AppSlug,
		// An empty app_slug means the sidecar described a global agent — opt in
		// explicitly so the gateway (which refuses to default to global) accepts
		// the pull→push roundtrip.
		Global: side.AppSlug == "",
	}
	if ref != "" {
		cur, err := gw.ResolveAgent(ctx, ref)
		if err != nil {
			return err
		}
		a, err := gw.UpdateAgent(ctx, cur.ID, body)
		if err != nil {
			return err
		}
		fmt.Printf("pushed (updated) %s (%s)\n", a.Slug, a.ID)
		return nil
	}
	a, err := gw.CreateAgent(ctx, body)
	if err != nil {
		return err
	}
	fmt.Printf("pushed (created) %s (slug %s, %s)\n", a.Name, a.Slug, a.ID)
	return nil
}
