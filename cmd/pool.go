package cmd

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Pool shows where your agents can run and what is free right now:
// your provider accounts (elastic, never busy) and every machine in your
// pool, yours or shared with you, as free / busy / offline.
//
//	cerver pool                     # the table
//	cerver pool --state free        # only free machines
//	cerver pool free                # print one free machine id (exit 1 if none)
//	cerver run --on "$(cerver pool free)" "…"
//	cerver pool share <compute_id>  # share your machine with the pool
//	cerver pool unshare <compute_id>
//	cerver pool invite [--new]      # the invite link (--new replaces it)
//	cerver pool add e2b             # connect a cloud provider (key from $E2B_API_KEY, a pipe, or typed hidden)
//	cerver pool --pool <pool_id>    # a pool you joined
func Pool(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("pool", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Emit raw JSON (for scripting and agents)")
	state := fs.String("state", "", "Only machines in this state: free, busy or offline")
	poolID := fs.String("pool", "", "A pool you joined (default: your own)")
	fresh := fs.Bool("new", false, "With invite: make a new link (the old one stops working)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return fmt.Errorf("no cerver credentials found — run cerver.ai/install.sh first")
	}
	gw := gateway.New(tok)

	switch sub {
	case "", "status", "ls", "list":
		return poolStatus(ctx, gw, *poolID, *state, *jsonOut)
	case "free":
		p, _, err := gw.GetPool(ctx, *poolID, "free")
		if err != nil {
			return err
		}
		for _, m := range p.Machines {
			if m.State == "free" && (m.Mine || m.Shared) {
				fmt.Println(m.ComputeID)
				return nil
			}
		}
		fmt.Fprintln(os.Stderr, "No machine is free right now. Use a provider (--on provider_e2b), or wait.")
		os.Exit(1)
	case "share", "unshare":
		if len(rest) != 1 {
			return fmt.Errorf("usage: cerver pool %s <compute_id>", sub)
		}
		target := ""
		if sub == "share" {
			p, _, err := gw.GetPool(ctx, *poolID, "")
			if err != nil {
				return err
			}
			target = p.PoolID
		}
		if err := gw.SharePoolMachine(ctx, rest[0], target); err != nil {
			return err
		}
		if sub == "share" {
			fmt.Printf("Shared %s with the pool. Members can run on it now.\n", rest[0])
		} else {
			fmt.Printf("%s is yours only again.\n", rest[0])
		}
		return nil
	case "add", "connect":
		if len(rest) != 1 {
			return fmt.Errorf("usage: cerver pool add <e2b|vercel|daytona|modal>   (key from $E2B_API_KEY etc., a pipe, or typed)")
		}
		return poolAdd(ctx, gw, strings.ToLower(rest[0]))
	case "invite":
		if *fresh {
			u, err := gw.NewPoolInvite(ctx)
			if err != nil {
				return err
			}
			fmt.Println(u)
			return nil
		}
		p, _, err := gw.GetPool(ctx, "", "")
		if err != nil {
			return err
		}
		if p.InviteURL == "" {
			u, err := gw.NewPoolInvite(ctx)
			if err != nil {
				return err
			}
			fmt.Println(u)
			return nil
		}
		fmt.Println(p.InviteURL)
		return nil
	default:
		return fmt.Errorf("unknown: cerver pool %s (try: cerver pool, free, share, unshare, invite, add)", sub)
	}
	return nil
}

func poolStatus(ctx context.Context, gw *gateway.Client, poolID, state string, jsonOut bool) error {
	p, raw, err := gw.GetPool(ctx, poolID, state)
	if err != nil {
		return err
	}
	if jsonOut {
		return encodeJSON(os.Stdout, raw)
	}

	title := p.Name
	if title == "" {
		title = "Your pool"
	}
	fmt.Println(title)
	if p.Say != "" {
		fmt.Println(p.Say)
	}

	if len(p.Providers) > 0 && poolID == "" {
		fmt.Println()
		fmt.Println("PROVIDERS  (elastic: a fresh computer each time, never busy)")
		w := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
		for _, pr := range p.Providers {
			fmt.Fprintf(w, "  %s\t--on provider_%s\t%s\n", pr.Name, pr.Provider, pr.BilledBy)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	fmt.Println()
	fmt.Println("MACHINES")
	if len(p.Machines) == 0 {
		if state != "" {
			fmt.Printf("  (none %s)\n", state)
		} else {
			fmt.Println("  (none — add one with `cerver connect` on that machine)")
		}
	} else {
		w := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  STATE\tNAME\tOWNER\tSHARED\tID\tNOTE")
		for _, m := range p.Machines {
			owner := m.Owner
			if m.Mine {
				owner = "you"
			}
			shared := "no"
			if m.Shared {
				shared = "yes"
			}
			note := ""
			if m.State == "busy" && len(m.Yours) > 0 {
				name := m.Yours[0].SessionID
				if m.Yours[0].Name != nil && *m.Yours[0].Name != "" {
					name = *m.Yours[0].Name
				}
				note = "your session: " + name + " (" + m.Yours[0].SessionID + ")"
			} else if m.State == "busy" {
				note = "someone else's session"
			} else if m.State == "offline" {
				note = "relay not reporting"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", stateMark(m.State), m.Name, owner, shared, m.ComputeID, note)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if p.Manage.Say != "" {
		fmt.Println()
		fmt.Println(p.Manage.Say)
	}
	if len(p.Manage.FreeNow) > 0 {
		fmt.Println(`  cerver run --on "$(cerver pool free)" "…"`)
	}
	if len(p.PoolsYouAreIn) > 0 && poolID == "" {
		fmt.Println()
		fmt.Println("Pools you joined:")
		for _, j := range p.PoolsYouAreIn {
			fmt.Printf("  %s  (cerver pool --pool %s)\n", j.Name, j.PoolID)
		}
	}
	return nil
}

// providerFields: what each cloud provider needs, and the env var it's usually in.
var providerFields = map[string][]struct{ field, env, what string }{
	"e2b":     {{"e2b_api_key", "E2B_API_KEY", "E2B API key (e2b.dev/dashboard → Keys)"}},
	"vercel":  {{"vercel_token", "VERCEL_TOKEN", "Vercel access token (vercel.com/account/tokens)"}},
	"daytona": {{"daytona_api_key", "DAYTONA_API_KEY", "Daytona API key"}},
	"modal": {
		{"modal_token_id", "MODAL_PROXY_TOKEN_ID", "Modal proxy token id"},
		{"modal_token_secret", "MODAL_PROXY_TOKEN_SECRET", "Modal proxy token secret"},
	},
}

// poolAdd connects a cloud provider account, so the pool can start computers
// on it. Keys come from the environment, a pipe, or a hidden prompt — never
// from a command-line argument, so they stay out of shell history.
func poolAdd(ctx context.Context, gw *gateway.Client, provider string) error {
	fields, ok := providerFields[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (try e2b, vercel, daytona or modal)", provider)
	}
	creds := map[string]string{}
	stdin := bufio.NewReader(os.Stdin)
	piped := false
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		piped = true
	}
	for _, f := range fields {
		v := strings.TrimSpace(os.Getenv(f.env))
		if v == "" {
			if !piped {
				fmt.Fprintf(os.Stderr, "%s (hidden): ", f.what)
				_ = sttyEcho(false)
			}
			line, _ := stdin.ReadString('\n')
			if !piped {
				_ = sttyEcho(true)
				fmt.Fprintln(os.Stderr)
			}
			v = strings.TrimSpace(line)
		}
		if v == "" {
			return fmt.Errorf("no %s given (set $%s or pipe it in)", f.what, f.env)
		}
		creds[f.field] = v
	}
	var out map[string]any
	if err := gw.Do(ctx, "POST", "/v2/account/providers", map[string]any{"provider": provider, "credentials": creds}, &out); err != nil {
		return err
	}
	fmt.Printf("Connected %s. Your pool can now start %s computers, billed to your own %s account.\n", provider, provider, provider)
	fmt.Printf("  cerver run --on provider_%s \"…\"\n", provider)
	return nil
}

// sttyEcho turns terminal echo on or off so a typed key isn't shown.
func sttyEcho(on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	c := exec.Command("stty", arg)
	c.Stdin = os.Stdin
	return c.Run()
}

func stateMark(s string) string {
	switch s {
	case "free":
		return "● free"
	case "busy":
		return "◐ busy"
	default:
		return "○ off"
	}
}
