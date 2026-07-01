package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Admin is the shared dispatcher behind the standalone `cerver-admin`
// operator binary. It exposes the owner-only /v2/admin/* endpoints; the
// gateway gates each to its operator allowlist, so a normal user's token
// gets a 403. Everything here is read-or-govern, never destructive without
// an id.
//
//	cerver-admin users                 # every signed-up account + activity
//	cerver-admin users --days 7        # window the usage sums
//	cerver-admin users --json          # raw JSON
//	cerver-admin disable <account_id>  # suspend an account
//	cerver-admin enable  <account_id>  # restore it
func Admin(args []string) error {
	sub := "users"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "users", "usage", "accounts":
		return adminUsers(rest)
	case "disable":
		return adminSetEnabled(rest, false)
	case "enable":
		return adminSetEnabled(rest, true)
	default:
		return fmt.Errorf("unknown admin subcommand %q (try: users | disable <id> | enable <id>)", sub)
	}
}

func adminUsers(args []string) error {
	fs := flag.NewFlagSet("admin users", flag.ContinueOnError)
	days := fs.Int("days", 30, "Window (days) the sessions/tokens sums cover.")
	jsonOut := fs.Bool("json", false, "Emit raw JSON instead of a table.")
	all := fs.Bool("all", false, "Show every account, including 0-activity test/CI rows.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return errors.New("no cerver credentials — run `curl -fsSL https://cerver.ai/install.sh | bash` first")
	}
	gw := gateway.New(tok)

	rows, err := gw.AdminUsage(ctx, *days)
	if err != nil {
		return ownerOnly(err)
	}

	if *jsonOut {
		return jsonEncode(os.Stdout, rows)
	}

	// Most recently active first; idle accounts (no last_active) sink.
	sort.SliceStable(rows, func(i, j int) bool {
		return lastActiveUnix(rows[j].LastActive) < lastActiveUnix(rows[i].LastActive)
	})

	// Classify so the summary can separate real users from trials / CI noise.
	var real, trial, test, idle int
	for _, r := range rows {
		switch classifyAccount(r.Email) {
		case "trial":
			trial++
		case "test":
			test++
		default:
			real++
		}
		if r.LastActive == nil {
			idle++
		}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tEMAIL\tKIND\tSESSIONS\tLLM TOKENS\tLAST ACTIVE\tACCOUNT ID")
	n := 0
	for _, r := range rows {
		kind := classifyAccount(r.Email)
		// By default hide the 0-activity test/CI accounts — they're not people.
		if !*all && kind == "test" && r.Sessions == 0 && r.LLMTokensTotal == 0 {
			continue
		}
		n++
		email := "—"
		if r.Email != nil {
			email = *r.Email
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n, email, kind,
			commas(r.Sessions), humanCount(r.LLMTokensTotal),
			shortDate(r.LastActive), r.AccountID)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	hidden := ""
	if !*all && test > 0 {
		hidden = fmt.Sprintf("  ·  %d test/CI hidden (--all to show)", test)
	}
	fmt.Printf("\n%d accounts  ·  %d real, %d trial, %d test  ·  %d idle%s\n",
		len(rows), real, trial, test, idle, hidden)
	return nil
}

func adminSetEnabled(args []string, enabled bool) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("need an account id: cerver admin disable|enable <account_id>")
	}
	accountID := fs.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return errors.New("no cerver credentials — run `curl -fsSL https://cerver.ai/install.sh | bash` first")
	}
	gw := gateway.New(tok)

	if err := gw.AdminSetAccountEnabled(ctx, accountID, enabled); err != nil {
		return ownerOnly(err)
	}
	verb := "disabled"
	if enabled {
		verb = "enabled"
	}
	fmt.Printf("account %s %s\n", accountID, verb)
	return nil
}

// ownerOnly rewrites the gateway's 403 ("Operator access required") into a
// plain sentence. `cerver admin` is gated server-side to accounts in the
// gateway's operator allowlist — this command is hidden from `cerver help`
// and useless to everyone else; the real enforcement lives on the gateway,
// not here.
func ownerOnly(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "Operator access") {
		return errors.New("cerver admin is owner-only — your account isn't an operator")
	}
	if strings.Contains(msg, "HTTP 401") {
		return errors.New("cerver admin needs an operator token — run `cerver login` as the owner account")
	}
	return err
}

// classifyAccount buckets an account by its email so the table can tell
// real users apart from anonymous trials and CI/test fixtures.
func classifyAccount(email *string) string {
	if email == nil {
		return "real"
	}
	e := strings.ToLower(*email)
	switch {
	case strings.HasPrefix(e, "trial_") || strings.HasSuffix(e, "@cerver.try"):
		return "trial"
	case strings.Contains(e, "@example.com") ||
		strings.Contains(e, "@cerver-test.local") ||
		strings.Contains(e, "@cerver-smoke.test") ||
		strings.Contains(e, "-test-") || strings.Contains(e, "-test@") ||
		strings.Contains(e, "smoke") || strings.Contains(e, "idempotent"):
		return "test"
	default:
		return "real"
	}
}

func lastActiveUnix(iso *string) int64 {
	if iso == nil {
		return 0
	}
	t, err := time.Parse(time.RFC3339, *iso)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func shortDate(iso *string) string {
	if iso == nil {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, *iso)
	if err != nil {
		return *iso
	}
	return t.UTC().Format("2006-01-02")
}

// commas renders a whole-number count with thousands separators.
func commas(n float64) string {
	s := fmt.Sprintf("%.0f", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
