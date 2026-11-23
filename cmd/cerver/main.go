// cerver — drive cerver sessions from the terminal.
//
// Subcommands (v1):
//
//	cerver run [--cli claude|codex|grok] [--on <compute>] [--bill api|sub] "prompt"
//	cerver compare "prompt" <cli> <compute> [<cli> <compute> …]
//	cerver computes [--json]
//
// Reads UA creds from ~/.cerver/infisical.env. Fetches CERVER_API_TOKEN
// (and provider keys when --bill api) at call time. Talks to
// gateway.cerver.ai directly — no local relay required for the CLI
// itself, though the relay is how you get a `cerver_local_provider`
// compute to point at.
package main

import (
	"github.com/eyal-gor/p_71_cerver_cli/internal/version"

	"fmt"
	"os"

	"github.com/eyal-gor/p_71_cerver_cli/cmd"
)

const helpText = `cerver — run AI agents on any compute, from your terminal

usage: cerver <command> [flags] [args]

commands:
  login      Sign in (device-code flow) and write ~/.cerver/cerver.env.
  logout     Revoke the local key server-side and delete cerver.env.
  connect    One-time setup: Claude Code bottom-bar indicator + gateway shims.
               Your subscriptions stay the default — nothing changes day to day.
  gateway    Route Claude Code through Cerver (metered, capped, redacted)
               or go direct on your subscription. Flips instantly, no restart.
                 cerver gateway on      # route through Cerver
                 cerver gateway off     # direct on your subscription
                 cerver gateway help    # what this means
  run        Send a single prompt to one CLI on one compute.
               cerver run --agent reviewer "review my last commit"
  agents     The agent board: awaiting input / working / completed, with a
               launch bar. ctrl+p lists everything you can do there.
                 cerver agents                       # the board
               Subcommands manage saved agent definitions (AGENTS.md +
               config); apply one to a run with: cerver run --agent <id>
                 cerver agents list                  # your saved definitions
                 cerver agents new --name "Reviewer" --md-file AGENTS.md --harness claude
                 cerver agents show <id>
                 cerver agents pull <id>        # write AGENTS.md + agent.json
                 cerver agents push [<id>]      # sync local files up
                 cerver agents rm <id>
  chat       Multi-turn conversation; resume with: cerver chat <sid>
  compare    Run the same prompt across multiple CLIs in parallel.
  split      Decompose one task across model tiers: a frontier planner splits
               it, each piece runs on the cheapest capable model, a merge step
               assembles the answer — with the cost vs frontier-only.
                 cerver split "write a landing page with pricing + FAQ"
                 cerver split --demo "…"   # also run it whole on the frontier
                                           # model + publish a /c/<id> compare
  computes   List the computes registered to your account.
  pool       Where your agents can run, and what is free right now.
               cerver pool                      # providers + machines: free / busy / offline
               cerver pool --state free         # only free machines
               cerver run --on "$(cerver pool free)" "…"   # run on a free machine
               cerver pool share <compute_id>   # share your machine (unshare to undo)
               cerver pool invite               # the link that lets someone join
  relay      This computer in your pool: agents can run on it.
               cerver relay install             # stay in the pool, also after restarts (no sudo)
               cerver relay                     # is it running?
               cerver relay uninstall           # leave the pool
  net       A private network between your own devices (WireGuard, direct).
               sudo cerver net install          # join, and stay on after restarts
               sudo cerver net up               # join until Ctrl-C
               sudo cerver net uninstall        # leave for good
               cerver net                       # your devices; reach them by name (ssh macbook)
               cerver net phone [--name iphone] # QR code for the WireGuard app on a phone
               cerver net rm <name|address>     # take a device out
  vm         A local VM for agents on an Apple computer (Apple's container tool).
               cerver vm                        # what's provisioned here
               sudo cerver vm install           # Apple's container tool (signed by Apple)
               cerver vm start                  # start it, build the agent image
               cerver vm login                  # your Claude login for agents in the VM
               cerver vm run "…"                # try it in this folder
  addons     Safety add-ons for this whole computer (filtering DNS).
               cerver addons                    # what's on here
               sudo cerver addons set kids      # off | malware | adult | kids
               cerver addons list               # every computer on your network
               cerver addons mac-mini safety kids
               cerver addons mac-mini focus work_hours youtube x   # off|work_hours|hour|always
  exit       Exit nodes: send a computer's internet through another one (e.g. home).
               cerver exit                                  # who offers, who uses which
               cerver exit offer mac-mini                   # mac-mini becomes an exit node
               cerver exit use macbook mac-mini             # macbook's internet goes through it
               cerver exit off macbook                      # macbook goes direct again
  forward    Reach an app on one of your machines from here, peer to peer.
               cerver forward MacBook-Air-2026 3456          # its port 3456 at localhost:3456 here
               cerver forward MacBook-Air-2026 3456 --local 8080
  projects       Manage your projects (per-account namespaces for sessions/keys/billing).
               cerver projects                          # list with this month's stats
               cerver projects create --name "Kompany" [--slug kompany]
               cerver projects set-vault --slug kompany --vault ifc_…
               cerver projects delete --slug kompany
  keys       Manage project-scoped API keys (every key belongs to one project).
  cerver keys                          # list (masked) + their project
               cerver keys create --project kompany [--label "prod server"]
               cerver keys delete --prefix ck_1a2b
  envs       Manage project environments + their repo bindings (CRUD).
               cerver envs                          # list across all projects
               cerver envs --project SLUG               # filter
               cerver envs create --project SLUG --slug prod [--default]
               cerver envs update --project SLUG --env prod --name "Prod"
               cerver envs delete --project SLUG --env prod
               cerver envs repos --project SLUG --env prod
               cerver envs repos add --project SLUG --env prod --url URL [--primary]
               cerver envs repos rm  --project SLUG --env prod --repo-id rep_…
  crons      Scheduled agent runs for a project (spend-capped + attributed).
               cerver crons --project SLUG                          # list
               cerver crons create --project SLUG --schedule "0 9 * * *" --prompt "…"
               cerver crons update <id> --project SLUG --schedule "…" # edit
               cerver crons disable <id> --project SLUG              # pause
               cerver crons run <id> --project SLUG                 # fire now
               cerver crons rm  <id> --project SLUG
  vaults     Manage your Infisical vaults (per-account secret connections).
               cerver vaults                        # list
               cerver vaults add --label N --client-id ID --client-secret SEC --project-id PID [--default]
               cerver vaults rename --id ifc_… --label NEW
               cerver vaults set-default --id ifc_…
               cerver vaults verify --id ifc_…
               cerver vaults delete --id ifc_…
  insights   Run the "read between the lines" agent over recent sessions —
               returns top user asks, stuck patterns, and suggested features.
                 cerver insights                  # across all projects
                 cerver insights --project SLUG
                 cerver insights --limit 50 --json
  sessions   List recent sessions.
  fleet      The agent board, spelled the old way — same as: cerver agents
               This is where the flags live (--plain, --watch, --json, --limit).
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
  update     Bring cerver, the Mac app, the relay and the network service
             to the latest release. Runs by itself every 6 hours too.
                 cerver update             # update now
                 cerver update --check     # what's installed, what's new
                 cerver update --source    # build the latest commit with Go
  uninstall  Take Cerver off this computer completely (account entries,
             services, sign-in, files), so a fresh install starts clean.
                 cerver uninstall
                 cerver uninstall --keep-account   # keep it in your pool/network list
  version    Print this program's version.
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

Gateway & bridge (keep working when a subscription hits its limit):
  cerver connect            one-time setup: terminal indicator + claude/codex shims
  cerver bridge             route new claude/codex launches via the Cerver Gateway
  cerver bridge off         back to your subscription
  cerver connect status     what's installed / routed
  The bottom bar in Claude Code always shows where requests go:
    Cerver · direct to anthropic          your subscription, untouched
    Cerver Gateway ⚡ active → anthropic   routed via Cerver: metered, capped
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
	case "split":
		err = cmd.Split(args)
	case "computes":
		err = cmd.Computes(args)
	case "pool", "pools":
		err = cmd.Pool(args)
	case "forward":
		err = cmd.Forward(args)
	case "net":
		err = cmd.Net(args)
	case "vm":
		err = cmd.VM(args)
	case "exit":
		err = cmd.Exit(args)
	case "addons":
		err = cmd.Addons(args)
	case "relay":
		err = cmd.Relay(args)
	case "envs", "env", "environments":
		err = cmd.Envs(args)
	case "vaults", "vault":
		err = cmd.Vaults(args)
	case "projects", "project":
		err = cmd.Projects(args)
	case "keys", "key":
		err = cmd.Keys(args)
	case "agents", "agent":
		err = cmd.Agents(args)
	case "crons", "cron":
		err = cmd.Crons(args)
	case "insights", "insight":
		err = cmd.Insights(args)
	case "sessions":
		err = cmd.Sessions(args)
	case "fleet":
		err = cmd.Fleet(args)
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
	case "connect":
		err = cmd.Connect(args)
	case "statusline":
		err = cmd.Statusline(args)
	case "gateway":
		err = cmd.Gateway(args)
	case "bridge":
		err = cmd.Bridge(args)
	case "daemon":
		err = cmd.Daemon(args)
	case "update", "self-update":
		err = cmd.Update(args)
	case "uninstall":
		err = cmd.Uninstall(args)
	case "version", "--version", "-v":
		fmt.Println("cerver " + version.Version)
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
