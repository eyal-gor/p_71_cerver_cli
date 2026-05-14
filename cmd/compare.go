package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
	"github.com/eyal-gor/p_71_cerver_cli/internal/output"
)

// Compare runs the same prompt through N CLIs in parallel and prints
// the answers side-by-side. Default fanout: claude + codex (skips
// grok unless explicitly requested, since it's api-only and would
// surprise users without a key).
func Compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	clisFlag := fs.String("clis", "claude,codex", "Comma-separated CLIs (subset of claude,codex,grok)")
	on := fs.String("on", "", "Compute to run on (default: first local relay)")
	billFlag := fs.String("bill", "", "Billing override. Global: `api` or `sub`. Per-CLI: `claude=sub,codex=api`")
	timeoutSec := fs.Int("timeout", 180, "Max seconds to wait for replies")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return errors.New("usage: cerver compare [flags] \"your prompt\"")
	}
	clis := strings.Split(*clisFlag, ",")
	for i, c := range clis {
		clis[i] = strings.TrimSpace(c)
	}

	billPerCLI, err := parseBillFlag(*billFlag, clis)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*timeoutSec)*time.Second+15*time.Second)
	defer cancel()

	cerverTok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if cerverTok == "" {
		return errors.New("no cerver credentials found — run cerver.ai/install.sh first")
	}
	// Lazy Infisical handle for the `api` billing path (vendor keys).
	// Nil here means we won't need Infisical for any of the CLIs picked;
	// runOneCLI initializes it on demand only when mode == "api".
	var inf *infisical.Client
	for _, m := range billPerCLI {
		if m == "api" {
			icfg, err := infisical.LoadConfig()
			if err != nil {
				return fmt.Errorf("--bill api needs Infisical for vendor keys: %w", err)
			}
			inf = infisical.New(icfg)
			break
		}
	}
	gw := gateway.New(cerverTok)

	computeID, err := pickCompute(ctx, gw, *on)
	if err != nil {
		return err
	}

	type result struct {
		cli     string
		reply   string
		usage   *gateway.Usage
		elapsed int
		mode    string
		err     error
	}
	results := make(chan result, len(clis))
	var wg sync.WaitGroup

	for _, c := range clis {
		c := c
		mode := billPerCLI[c]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := runOneCLI(ctx, inf, gw, c, computeID, prompt, mode, *timeoutSec)
			results <- r
		}()
	}
	wg.Wait()
	close(results)

	// Drain into a stable order matching the user's --clis arg.
	resMap := map[string]result{}
	for r := range results {
		resMap[r.cli] = r
	}
	for _, c := range clis {
		r := resMap[c]
		if r.err != nil {
			fmt.Printf("==== %s (error) ====\n%s\n\n", c, r.err)
			continue
		}
		fmt.Println(output.Header(c, r.elapsed, r.mode, r.usage))
		fmt.Println(r.reply)
		fmt.Println()
	}
	return nil
}

func runOneCLI(ctx context.Context, inf *infisical.Client, gw *gateway.Client,
	cli, computeID, prompt, mode string, timeoutSec int) (out struct {
	cli, reply string
	usage      *gateway.Usage
	elapsed    int
	mode       string
	err        error
}) {
	out.cli = cli
	out.mode = mode
	envInject := map[string]string{}
	if mode == "api" {
		keyName := apiKeyEnvFor(cli)
		v, err := inf.Get(ctx, keyName)
		if err != nil {
			out.err = err
			return
		}
		if v == "" {
			out.err = fmt.Errorf("%s api mode but %s isn't in vault", cli, keyName)
			return
		}
		envInject[keyName] = v
	}

	metadata := map[string]any{"cli_tool": cli}
	if len(envInject) > 0 {
		metadata["env"] = envInject
	}
	sid, err := gw.CreateSession(ctx, gateway.SessionCreate{
		SessionType: "coding",
		Compute:     map[string]any{"compute_id": computeID},
		Task:        prompt,
		Workload:    "coding",
		SessionName: "cli-compare-" + cli,
		Metadata:    metadata,
	})
	if err != nil {
		out.err = err
		return
	}
	if err := gw.SendInput(ctx, sid, prompt); err != nil {
		out.err = err
		return
	}
	start := time.Now()
	s, err := gw.WaitForReply(ctx, sid,
		time.Duration(timeoutSec)*time.Second,
		8*time.Second)
	if err != nil {
		out.err = err
		return
	}
	out.reply = s.LastAssistantText()
	out.usage = s.Usage()
	out.elapsed = int(time.Since(start).Seconds())
	return
}

// parseBillFlag accepts either a global value ("api" / "sub") or a
// per-CLI csv ("claude=sub,codex=api"). Returns a fully resolved
// {cli: mode} map for every CLI in `clis`, falling back to the vendor
// default when unspecified.
func parseBillFlag(raw string, clis []string) (map[string]string, error) {
	out := map[string]string{}
	for _, c := range clis {
		// Vendor default.
		if c == "grok" {
			out[c] = "api"
		} else {
			out[c] = "subscription"
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	if !strings.Contains(raw, "=") {
		// Global override.
		mode, err := normalizeMode(raw)
		if err != nil {
			return nil, err
		}
		for _, c := range clis {
			if c == "grok" && mode == "subscription" {
				return nil, errors.New("grok has no subscription mode — drop --bill or use api")
			}
			out[c] = mode
		}
		return out, nil
	}
	for _, kv := range strings.Split(raw, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --bill entry %q", kv)
		}
		cli := strings.TrimSpace(parts[0])
		mode, err := normalizeMode(parts[1])
		if err != nil {
			return nil, err
		}
		if cli == "grok" && mode == "subscription" {
			return nil, errors.New("grok has no subscription mode")
		}
		out[cli] = mode
	}
	return out, nil
}

func normalizeMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "subscription", "sub":
		return "subscription", nil
	case "api":
		return "api", nil
	default:
		return "", fmt.Errorf("unknown billing mode %q (use subscription / sub / api)", s)
	}
}
