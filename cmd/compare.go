package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
	"github.com/eyal-gor/p_71_cerver_cli/internal/output"
)

// Compare runs the same prompt across N CLI/compute pairs in parallel
// and prints the answers side-by-side.
//
// Usage:
//
//	cerver compare "<prompt>" <cli> <compute> [<cli> <compute> …]
//
// Example:
//
//	cerver compare "explain Raft leader election" \
//	  claude mac-mini \
//	  codex mac-mini \
//	  grok  provider_vercel
//
// Positional after the prompt is a flat sequence of (cli, compute)
// pairs. Each pair gets its own goroutine, its own session, its own
// compute — so users can route, say, codex to a beefier box than
// claude in the same run. Repeating the same CLI is allowed (e.g.
// `claude mac-mini claude macbook` for an A/B on the same model).
// Compute query strings go through the same resolver as `--on` did
// before: nickname, prefix, compute_id, or compute_label match.
//
// `--bill` and `--models` still take global or per-CLI csv values —
// they're rarely changed, so we keep them as flags rather than packing
// more positionals.
func Compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	billFlag := fs.String("bill", "", "Billing override. Global: `api` or `sub`. Per-CLI: `claude=sub,codex=api`")
	modelsFlag := fs.String("models", "", "Model override. Global: `sonnet`. Per-CLI: `claude=opus,codex=gpt-5-codex`. Empty = each CLI's local default.")
	timeoutSec := fs.Int("timeout", 180, "Max seconds to wait for replies")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 1 {
		return errors.New(`usage: cerver compare "<prompt>" <cli> <compute> [<cli> <compute> …]`)
	}
	prompt := strings.TrimSpace(rest[0])
	if prompt == "" {
		return errors.New("compare: prompt is empty")
	}

	pairs := rest[1:]
	if len(pairs) == 0 {
		return errors.New("compare: specify at least one <cli> <compute> pair (e.g. `claude mac-mini`)")
	}
	if len(pairs)%2 != 0 {
		return fmt.Errorf("compare: uneven trailing args — must be <cli> <compute> pairs, got %d tokens after the prompt", len(pairs))
	}

	// Order matters: output preserves the order pairs appeared on the
	// command line, so `claude codex grok` and `grok claude codex` give
	// the same answers but rendered top-to-bottom in the user's order.
	type entry struct{ cli, computeQuery string }
	entries := make([]entry, 0, len(pairs)/2)
	clis := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		cli := strings.TrimSpace(pairs[i])
		cq := strings.TrimSpace(pairs[i+1])
		if cli == "" || cq == "" {
			return fmt.Errorf("compare: blank cli or compute in pair %d", i/2+1)
		}
		entries = append(entries, entry{cli, cq})
		clis = append(clis, cli)
	}

	billPerCLI, err := parseBillFlag(*billFlag, clis)
	if err != nil {
		return err
	}
	modelPerCLI := parseModelsFlag(*modelsFlag, clis)

	// The timeout flag is per compare leg. When several legs target the
	// same compute we run them sequentially, so the outer context must
	// cover the longest per-compute group instead of only one leg.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*timeoutSec*len(entries))*time.Second+30*time.Second)
	defer cancel()

	cerverTok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if cerverTok == "" {
		return errors.New("no cerver credentials found — run cerver.ai/install.sh first")
	}
	gw := gateway.New(cerverTok)

	// Resolve each pair's compute query → compute_id once, cache repeats
	// so two pairs on the same machine don't double-hit /v2/computes.
	// The map is keyed by position (we may have the same cli twice with
	// different computes), so we track resolved IDs in a parallel slice.
	resolvedComputeIDs := make([]string, len(entries))
	cache := map[string]string{}
	for i, e := range entries {
		if hit, ok := cache[e.computeQuery]; ok {
			resolvedComputeIDs[i] = hit
			continue
		}
		id, err := pickCompute(ctx, gw, e.computeQuery)
		if err != nil {
			return fmt.Errorf("compute for %s=%q: %w", e.cli, e.computeQuery, err)
		}
		cache[e.computeQuery] = id
		resolvedComputeIDs[i] = id
	}

	// One result per entry. Indexed by position so duplicate CLIs
	// (e.g. `claude mac-mini claude macbook`) each get their own row.
	type result struct {
		idx     int
		cli     string
		compute string
		reply   string
		usage   *gateway.Usage
		elapsed int
		mode    string
		err     error
	}
	results := make(chan result, len(entries))
	var wg sync.WaitGroup

	// Run one compare leg at a time per compute. A local relay can run
	// multiple CLIs, but starting Claude, Codex, and Grok at once on the
	// same machine is fragile: one leg can starve and never publish its
	// final assistant text. Different computes still run in parallel.
	byCompute := map[string][]int{}
	for i := range entries {
		byCompute[resolvedComputeIDs[i]] = append(byCompute[resolvedComputeIDs[i]], i)
	}
	for _, indexes := range byCompute {
		sort.SliceStable(indexes, func(i, j int) bool {
			left := entries[indexes[i]].cli
			right := entries[indexes[j]].cli
			return launchPriority(left, billPerCLI[left]) < launchPriority(right, billPerCLI[right])
		})
	}

	for _, indexes := range byCompute {
		indexes := indexes
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, i := range indexes {
				e := entries[i]
				mode := billPerCLI[e.cli]
				model := modelPerCLI[e.cli]
				computeID := resolvedComputeIDs[i]
				legCtx, cancelLeg := context.WithTimeout(ctx,
					time.Duration(*timeoutSec)*time.Second+15*time.Second)
				r := runOneCLI(legCtx, gw, e.cli, computeID, prompt, mode, model, *timeoutSec)
				cancelLeg()
				results <- result{
					idx:     i,
					cli:     e.cli,
					compute: e.computeQuery,
					reply:   r.reply,
					usage:   r.usage,
					elapsed: r.elapsed,
					mode:    r.mode,
					err:     r.err,
				}
			}
		}()
	}
	wg.Wait()
	close(results)

	// Drain into a stable order matching the user's pair sequence on
	// the command line. Index-based ordering also covers the duplicate-
	// CLI case where map-by-name would collapse rows.
	ordered := make([]result, len(entries))
	for r := range results {
		ordered[r.idx] = r
	}
	for _, r := range ordered {
		// Header takes the bare cli name (apiKeyEnvFor + Cost look it
		// up internally), so the compute label rides on a second line.
		if r.err != nil {
			fmt.Printf("==== %s @ %s (error) ====\n%s\n\n", r.cli, r.compute, r.err)
			continue
		}
		fmt.Println(output.Header(r.cli, r.elapsed, r.mode, r.usage))
		fmt.Printf("  on %s\n", r.compute)
		fmt.Println(r.reply)
		fmt.Println()
	}
	return nil
}

func launchPriority(cli, mode string) int {
	if cli == "grok" || mode == "api" {
		return 0
	}
	return 1
}

func runOneCLI(ctx context.Context, gw *gateway.Client,
	cli, computeID, prompt, mode, model string, timeoutSec int) (out struct {
	cli, reply string
	usage      *gateway.Usage
	elapsed    int
	mode       string
	err        error
}) {
	out.cli = cli
	out.mode = mode

	// `complete_on_exit` tells the relay to close the agent record when
	// the CLI process ends instead of parking it in `paused` for a
	// possible --resume. `cerver compare` is one-shot by design, so a
	// next compare against the same (cli, compute) pair shouldn't land
	// on a stale paused agent and resume into its half-written JSONL.
	metadata := map[string]any{"cli_tool": cli, "complete_on_exit": true}
	if model != "" {
		metadata["cli_model"] = model
	}
	sid, err := gw.CreateSession(ctx, gateway.SessionCreate{
		SessionType: "coding",
		Compute:     map[string]any{"compute_id": computeID},
		Task:        prompt,
		Workload:    "coding",
		SessionName: "cli-run",
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
