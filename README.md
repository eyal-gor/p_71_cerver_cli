# cerver

A standalone CLI for [cerver.ai](https://cerver.ai) — run AI coding agents (Claude Code, Codex, Grok) on any compute, from your terminal.

This is the "extracted from the skill" path: the same bash procedure the `/cerver` Claude Code skill executes, packaged as a single static binary you can pipe into shell scripts, CI, cron, or any non-Claude-Code workflow.

## Status

**v0.1 — three verbs that work end-to-end:** `run`, `compare`, `computes`.

Coming in v0.2: `login` (interactive `~/.cerver/infisical.env` bootstrap), `sessions` (list recent), `peek` (one-screen snapshot), `show` (transcript with `--follow`), `move` (change compute mid-session).

## Install

For now (until goreleaser + Homebrew tap land):

```bash
go install github.com/eyal-gor/p_71_cerver_cli/cmd/cerver@latest
# or, in a clone:
go build -o cerver ./cmd/cerver && mv cerver /usr/local/bin/
```

Both produce a binary called `cerver` on your `$PATH`.

You'll also need `~/.cerver/infisical.env` (created by the cerver relay installer, or by hand) with these keys:

```
INFISICAL_CLIENT_ID=<UA client id>
INFISICAL_TOKEN=<UA client secret>
INFISICAL_PROJECT_ID=<workspace id>
INFISICAL_ENV=prod
```

The CLI fetches `CERVER_API_TOKEN` (always) and `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `XAI_API_KEY` (only when you pass `--bill api`) from that vault. Nothing else gets read locally.

## Usage

```bash
cerver run "what's the diff between v0 and main?"
cerver run --cli codex --bill api "summarize today's commits"
cerver run --on mac-mini "..."

cerver compare "explain Raft leader election"
cerver compare --clis claude,codex,grok "..."
cerver compare --bill claude=sub,codex=api "..."

cerver computes
cerver computes --json
```

Each reply is prefixed with a one-line header showing elapsed time, billing mode, and (once the relay supports usage push) token counts + cost:

```
==== claude (3s · subscription · local OAuth · 145 in / 487 out · $0.0076 rate-card, not billed) ====
==== codex  (4s · api · OPENAI_API_KEY · 145 in / 351 out · $0.0037 billed) ====
```

## Billing modes

| CLI | `subscription` (default for claude/codex) | `api` (default for grok) |
|---|---|---|
| claude | local `claude login` → Claude Max / Pro / Team | `ANTHROPIC_API_KEY` from your vault |
| codex | local `codex login` → ChatGPT Plus/Pro | `OPENAI_API_KEY` |
| grok | n/a — grok has no subscription path | `XAI_API_KEY` |

Override per call with `--bill api`, `--bill sub`, or per-CLI: `--bill claude=sub,codex=api`.

## How it works

```
cerver run "X"
  │
  ├─ Read ~/.cerver/infisical.env, exchange UA creds → Infisical access token
  ├─ Fetch CERVER_API_TOKEN (and ANTHROPIC_API_KEY / OPENAI_API_KEY / XAI_API_KEY if --bill api)
  ├─ POST gateway.cerver.ai/v2/sessions  { cli_tool, compute_id, task, metadata.env }
  ├─ POST /v2/sessions/<id>/input        { content: prompt, role: user }
  ├─ Poll /v2/sessions/<id>?since=N      until an assistant text entry lands
  └─ Print  ==== <cli> (Ns · mode · ...) ====
            <reply>
```

`compare` does the same thing N times concurrently (one goroutine per CLI), then prints results in `--clis` order so the output is stable.

## Why this exists

The cerver skill (`~/.claude/skills/cerver/SKILL.md`) is great when you're already in Claude Code. The CLI exists because cerver's actual value — fan out a prompt across CLIs and computes, compare answers, run agents in shell pipelines — needs to work *outside* an assistant too: in cron, CI, Makefiles, GitHub Actions, ad-hoc scripts.

Both [Claude Code](https://www.anthropic.com/claude-code) and [Codex CLI](https://github.com/openai/codex) independently suggested this CLI when asked whether to ship one; the skill and the CLI now share the same gateway, the same auth, and the same billing semantics.

## Roadmap

- v0.2: `login`, `sessions list/show`, `peek`, `move`
- v0.3: goreleaser → darwin/linux binaries, Homebrew tap, install.sh integration
- v0.4: account-level billing prefs (reads `GET /v2/accounts/me/billing` when the gateway endpoint ships)
- v1.0: stream output as it lands instead of polling for the full reply
