// cerver — drive cerver sessions from the terminal.
//
// Subcommands (v1):
//   cerver run [--cli claude|codex|grok] [--on <compute>] [--bill api|sub] "prompt"
//   cerver compare "prompt" <cli> <compute> [<cli> <compute> …]
//   cerver computes [--json]
//
// Reads UA creds from ~/.cerver/infisical.env. Fetches CERVER_API_TOKEN
// (and provider keys when --bill api) at call time. Talks to
// gateway.cerver.ai directly — no local relay required for the CLI
// itself, though the relay is how you get a `cerver_local_provider`
// compute to point at.
package main

import (
	"fmt"
	"os"

	"github.com/eyal-gor/p_71_cerver_cli/cmd"
)

const helpText = `cerver — run AI agents on any compute, from your terminal

usage: cerver <command> [flags] [args]

commands:
  login      Sign in (device-code flow) and write ~/.cerver/cerver.env.
  logout     Revoke the local key server-side and delete cerver.env.
  run        Send a single prompt to one CLI on one compute.
               cerver run --agent reviewer "review my last commit"
  agents     Save reusable agent definitions (AGENTS.md + config). Apply
               one to a run with: cerver run --agent <id>
                 cerver agents                       # list
                 cerver agents new --name "Reviewer" --md-file AGENTS.md --harness claude
                 cerver agents show <id>
                 cerver agents pull <id>        # write AGENTS.md + agent.json
                 cerver agents push [<id>]      # sync local files up
                 cerver agents rm <id>
  chat       Multi-turn conversation; resume with: cerver chat <sid>
  compare    Run the same prompt across multiple CLIs in parallel.
  computes   List the computes registered to your account.
  apps       Manage your apps (per-account namespaces for sessions/keys/billing).
               cerver apps                          # list with this month's stats
               cerver apps create --name "Kompany" [--slug kompany]
               cerver apps set-vault --slug kompany --vault ifc_…
               cerver apps delete --slug kompany
  keys       Manage app-scoped API keys (every key belongs to one app).
               cerver keys                          # list (masked) + their app
               cerver keys create --app kompany [--label "prod server"]
               cerver keys delete --prefix ck_1a2b
  envs       Manage app environments + their repo bindings (CRUD).
               cerver envs                          # list across all apps
               cerver envs --app SLUG               # filter
               cerver envs create --app SLUG --slug prod [--default]
               cerver envs update --app SLUG --env prod --name "Prod"
               cerver envs delete --app SLUG --env prod
               cerver envs repos --app SLUG --env prod
               cerver envs repos add --app SLUG --env prod --url URL [--primary]
               cerver envs repos rm  --app SLUG --env prod --repo-id rep_…
  vaults     Manage your Infisical vaults (per-account secret connections).
               cerver vaults                        # list
               cerver vaults add --label N --client-id ID --client-secret SEC --project-id PID [--default]
               cerver vaults rename --id ifc_… --label NEW
               cerver vaults set-default --id ifc_…
               cerver vaults verify --id ifc_…
               cerver vaults delete --id ifc_…
  insights   Run the "read between the lines" agent over recent sessions —
               returns top user asks, stuck patterns, and suggested features.
                 cerver insights                  # across all apps
                 cerver insights --app SLUG
                 cerver insights --limit 50 --json
  admin      Owner-only: inspect + govern all accounts.
               cerver admin users                 # every account + activity
               cerver admin users --days 7 --all  # window sums, show test rows
               cerver admin disable <account_id>  # suspend an account
               cerver admin enable  <account_id>  # restore it
  sessions   List recent sessions.
  show       Print a session's full transcript (--follow to stream).
  peek       One-screen snapshot of a session (status + last reply).
  move       Move a live session to a different compute.
  billing    Show this month's cerver bill — service + database fees.
  suggestions Manage the cerver suggestion box:
               list (default) | new "summary"
  test       Run a saved test (one prompt across all three CLIs) and
               check the responses against simple expectations.
               Tests live as JSON files in ~/.cerver/tests/. First
               invocation seeds a starter test ("01_rate_limiter").
                 cerver test               # list tests
                 cerver test 01            # run by id-prefix match
                 cerver test --all         # run every test
                 cerver test diagnose <provider>
                                           # probe provider readiness
                                           # (e.g. vercel)
  update     Reinstall cerver from the latest commit on main. Uses
               'go install' and lands the new binary in the same
               directory the current one runs from.
                 cerver update             # upgrade in place
                 cerver update --verbose   # stream go output
                 cerver update --dry-run   # show what'd happen
  help       Show this message.

examples:
  cerver run "what's the time zone of UTC+3?"
  cerver run --cli codex --bill api "summarize today's commits"

  cerver compare "explain Raft leader election" \
    claude mac-mini \
    codex  mac-mini \
    grok   provider_vercel

  cerver compare "fix the failing test" \
    claude mac-mini \
    claude macbook        # A/B the same CLI on two boxes

  cerver computes
  cerver computes --json

config:
  ~/.cerver/infisical.env  Universal Auth creds for your secret vault.
                           Generated by cerver-relay's installer or
                           created by hand. Holds CERVER_API_TOKEN +
                           optional ANTHROPIC_API_KEY / OPENAI_API_KEY /
                           XAI_API_KEY (only needed for --bill api).

billing modes:
  subscription (default for claude / codex): uses your local 'claude login'
                                              or 'codex login' OAuth.
                                              No per-token charge.
  api          (default for grok):           uses the vendor's API key
                                              from your vault. Billed
                                              per token.

  --bill sub     force subscription on this call
  --bill api     force api key on this call
  --bill <cli>=<mode>,<cli>=<mode>  per-CLI override for compare
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(helpText)
		os.Exit(0)
	}
	cmdName := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmdName {
	case "login":
		err = cmd.Login(args)
	case "logout":
		err = cmd.Logout(args)
	case "run":
		err = cmd.Run(args)
	case "chat":
		err = cmd.Chat(args)
	case "compare":
		err = cmd.Compare(args)
	case "computes":
		err = cmd.Computes(args)
	case "envs", "env", "environments":
		err = cmd.Envs(args)
	case "vaults", "vault":
		err = cmd.Vaults(args)
	case "apps", "app":
		err = cmd.Apps(args)
	case "keys", "key":
		err = cmd.Keys(args)
	case "agents", "agent":
		err = cmd.Agents(args)
	case "insights", "insight":
		err = cmd.Insights(args)
	case "admin":
		err = cmd.Admin(args)
	case "sessions":
		err = cmd.Sessions(args)
	case "show":
		err = cmd.Show(args)
	case "peek":
		err = cmd.Peek(args)
	case "move":
		err = cmd.Move(args)
	case "billing", "bill":
		err = cmd.Billing(args)
	case "suggestions", "suggest":
		err = cmd.Suggestions(args)
	case "test":
		err = cmd.Test(args)
	case "update", "self-update":
		err = cmd.Update(args)
	case "help", "-h", "--help":
		fmt.Print(helpText)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmdName)
		fmt.Print(helpText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
