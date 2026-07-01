// cerver-admin — the operator CLI for cerver.
//
// A separate binary on purpose: these are owner-only account-governance
// commands, gated server-side to the gateway's operator allowlist
// (CERVER_OWNER_ACCOUNT_IDS). It is NOT part of the public `cerver`
// install — you build/install it yourself as the operator. A non-owner
// running it just gets "owner-only" from the gateway.
//
// It reuses the same ~/.cerver credentials as `cerver` (LoadCerverToken),
// so once you're logged in as the owner account, it just works.
package main

import (
	"fmt"
	"os"

	"github.com/eyal-gor/p_71_cerver_cli/cmd"
)

const helpText = `cerver-admin — operator CLI for cerver (owner-only)

usage: cerver-admin <command> [flags] [args]

commands:
  users      Every signed-up account with its activity.
               cerver-admin users                 # table, real/trial/test
               cerver-admin users --days 7        # window the usage sums
               cerver-admin users --all           # include test/CI rows
               cerver-admin users --json          # raw JSON for scripting
  disable    Suspend an account.
               cerver-admin disable <account_id>
  enable     Restore a suspended account.
               cerver-admin enable <account_id>
  help       Show this message.

auth:
  Uses your ~/.cerver credentials (same as the cerver CLI). The gateway
  gates every command to its operator allowlist — non-owners are refused.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(helpText)
		os.Exit(0)
	}
	switch os.Args[1] {
	case "help", "-h", "--help":
		fmt.Print(helpText)
		return
	}
	// Everything else is an admin subcommand — hand the full arg list to
	// the shared dispatcher (users | disable | enable), which defaults to
	// `users` when the first token is a flag.
	if err := cmd.Admin(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
