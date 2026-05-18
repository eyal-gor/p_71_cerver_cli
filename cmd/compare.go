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
	on := fs.String("on", "", "Compute. Global: `mac-mini`. Per-CLI: `claude=macbook,codex=mac-mini,grok=provider_vercel`. Empty = first ready local relay.")
	billFlag := fs.String("bill", "", "Billing override. Global: `api` or `sub`. Per-CLI: `claude=sub,codex=api`")
	modelsFlag := fs.String("models", "", "Model override. Global: `sonnet`. Per-CLI: `claude=opus,codex=gpt-5-codex`. Empty = each CLI's local default.")
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
	modelPerCLI := parseModelsFlag(*modelsFlag, clis)

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

	computePerCLI, err := resolveComputeFlag(ctx, gw, *on, clis)
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
		model := modelPerCLI[c]
		computeID := computePerCLI[c]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := runOneCLI(ctx, inf, gw, c, computeID, prompt, mode, model, *timeoutSec)
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
	cli, computeID, prompt, mode, model string, timeoutSec int) (out struct {
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
	if model != "" {
		metadata["cli_model"] = model
	}
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

// resolveComputeFlag accepts either a global compute query
// ("mac-mini") or a per-CLI csv
// ("claude=macbook,codex=mac-mini,grok=provider_vercel"). Returns a
// fully resolved {cli: compute_id} map for every CLI in `clis`. CLIs
// not named in the per-CLI form fall back to pickCompute's default
// (first ready local-relay compute) — same behavior as a bare --on.
//
// Resolution happens in one place so the per-CLI goroutines below see
// concrete compute_ids and don't each pay a /v2/computes round trip.
func resolveComputeFlag(ctx context.Context, gw *gateway.Client, raw string, clis []string) (map[string]string, error) {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)

	if raw == "" || !strings.Contains(raw, "=") {
		// Single value (or empty) — resolve once, apply to every CLI.
		// pickCompute owns the empty-string-means-default branch.
		id, err := pickCompute(ctx, gw, raw)
		if err != nil {
			return nil, err
		}
		for _, c := range clis {
			out[c] = id
		}
		return out, nil
	}

	// Per-CLI: parse explicit entries, then fill any CLI not named with
	// the auto-picked default. Cache per query so two entries pointing
	// at the same compute don't double-call the API.
	cache := map[string]string{}
	resolveOne := func(q string) (string, error) {
		if hit, ok := cache[q]; ok {
			return hit, nil
		}
		id, err := pickCompute(ctx, gw, q)
		if err != nil {
			return "", err
		}
		cache[q] = id
		return id, nil
	}

	for _, kv := range strings.Split(raw, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --on entry %q (use cli=compute)", kv)
		}
		cli := strings.TrimSpace(parts[0])
		query := strings.TrimSpace(parts[1])
		id, err := resolveOne(query)
		if err != nil {
			return nil, fmt.Errorf("--on %s=%s: %w", cli, query, err)
		}
		out[cli] = id
	}

	// Fill in defaults for unnamed CLIs.
	var defaultID string
	for _, c := range clis {
		if _, ok := out[c]; ok {
			continue
		}
		if defaultID == "" {
			id, err := resolveOne("")
			if err != nil {
				return nil, err
			}
			defaultID = id
		}
		out[c] = defaultID
	}
	return out, nil
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

// parseModelsFlag accepts either a global model ("sonnet") applied to
// every CLI or per-CLI ("claude=opus,codex=gpt-5-codex"). Returns a
// {cli: model} map for every CLI in `clis`, with empty string when no
// override is requested for that CLI — relay treats empty as "use the
// CLI's local default."
func parseModelsFlag(raw string, clis []string) map[string]string {
	out := map[string]string{}
	for _, c := range clis {
		out[c] = ""
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	if !strings.Contains(raw, "=") {
		for _, c := range clis {
			out[c] = raw
		}
		return out
	}
	for _, kv := range strings.Split(raw, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out
}
