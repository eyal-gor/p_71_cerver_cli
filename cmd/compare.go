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

// Compare runs the same prompt across N (harness, model, compute)
// competitors in parallel and prints the answers side-by-side.
//
// Usage:
//
//	cerver compare "<prompt>" <cli>[/model] <compute> [<cli>[/model] <compute> …]
//
// Examples:
//
//	# different harnesses
//	cerver compare "explain Raft leader election" \
//	  claude mac-mini \
//	  codex mac-mini \
//	  grok  provider_vercel
//
//	# same harness, different model — opus 4.8 vs 4.7
//	cerver compare "implement the rate limiter" \
//	  claude/opus-4.8 mac-mini \
//	  claude/opus-4.7 mac-mini
//
// Positional after the prompt is a flat sequence of (competitor,
// compute) pairs. The competitor token is `harness[/model]`: a bare
// `claude` uses the CLI's local default model; `claude/opus-4.7` pins
// the model. Each pair gets its own goroutine, session, and compute,
// so two entries of the same harness can run different models — the
// model rides with the entry instead of being keyed by harness name.
// Compute query strings go through the same resolver as `--on` did
// before: nickname, prefix, compute_id, or compute_label match.
//
// `--bill` and `--models` still take global or per-CLI csv values; a
// per-token `/model` takes precedence over `--models` for that entry.
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
		return errors.New(`usage: cerver compare "<prompt>" <cli>[/model] <compute> [<cli>[/model] <compute> …]`)
	}
	prompt := strings.TrimSpace(rest[0])
	if prompt == "" {
		return errors.New("compare: prompt is empty")
	}

	pairs := rest[1:]
	if len(pairs) == 0 {
		return errors.New("compare: specify at least one <cli>[/model] <compute> pair (e.g. `claude mac-mini` or `claude/opus-4.7 mac-mini`)")
	}
	if len(pairs)%2 != 0 {
		return fmt.Errorf("compare: uneven trailing args — must be <cli>[/model] <compute> pairs, got %d tokens after the prompt", len(pairs))
	}

	// Order matters: output preserves the order pairs appeared on the
	// command line, so `claude codex grok` and `grok claude codex` give
	// the same answers but rendered top-to-bottom in the user's order.
	// The competitor token is `harness[/model]` — the model rides with
	// the entry so two same-harness entries (opus-4.8 vs opus-4.7) don't
	// collapse onto one harness-keyed model.
	type entry struct{ cli, model, computeQuery string }
	entries := make([]entry, 0, len(pairs)/2)
	clis := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		tok := strings.TrimSpace(pairs[i])
		cq := strings.TrimSpace(pairs[i+1])
		if tok == "" || cq == "" {
			return fmt.Errorf("compare: blank competitor or compute in pair %d", i/2+1)
		}
		cli, model := splitCLIModel(tok)
		if cli == "" {
			return fmt.Errorf("compare: blank harness in pair %d (%q)", i/2+1, tok)
		}
		entries = append(entries, entry{cli, model, cq})
		clis = append(clis, cli)
	}

	billPerCLI, err := parseBillFlag(*billFlag, clis)
	if err != nil {
		return err
	}
	modelPerCLI := parseModelsFlag(*modelsFlag, clis)

	// The timeout flag is per compare leg. Legs run concurrently (one
	// goroutine each), so a per-leg ceiling would suffice — but we keep a
	// generous outer budget so a slow leg plus its retries still fits well
	// inside the parent context.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*timeoutSec*len(entries))*time.Second+30*time.Second)
	defer cancel()

	cerverTok, err := infisical.LoadRunToken(ctx)
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
		model   string
		compute string
		reply   string
		usage   *gateway.Usage
		elapsed int // model run time (from the relay) — shown in the header
		legWall int // end-to-end wall for this leg (create + run + poll)
		mode    string
		err     error
	}
	results := make(chan result, len(entries))
	var wg sync.WaitGroup

	// True parallel: one goroutine per entry. The earlier "sequential
	// per compute" workaround (4608b46) papered over a relay-side
	// concurrency bug — three concurrent /run requests serialized in
	// the cerver_connect WS read loop, and the second + third never
	// got their CLI subprocess spawned before the upstream timed out.
	// That's fixed in the relay now (dcbbac2 — `asyncio.create_task`
	// per inbound request), so this client can run the legs in true
	// parallel: total wall time = slowest CLI, not sum.
	//
	// Announce the fan-out up front, then print a live ✓/✗ line as each
	// leg lands (in completion order — that's what makes the parallelism
	// visible; the side-by-side blocks below stay in command-line order).
	// Effective model per entry: a per-token `/model` wins; otherwise
	// fall back to the --models flag (keyed by harness); empty means the
	// relay uses the CLI's local default.
	effModels := make([]string, len(entries))
	legLabels := make([]string, len(entries))
	for i, e := range entries {
		m := e.model
		if m == "" {
			m = modelPerCLI[e.cli]
		}
		effModels[i] = m
		legLabels[i] = legLabel(e.cli, m, e.computeQuery)
	}
	fmt.Printf("→ running %d agents in parallel: %s\n", len(entries), strings.Join(legLabels, ", "))

	startAll := time.Now()
	for i := range entries {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := entries[i]
			mode := billPerCLI[e.cli]
			model := effModels[i]
			computeID := resolvedComputeIDs[i]
			legCtx, cancelLeg := context.WithTimeout(ctx,
				time.Duration(*timeoutSec)*time.Second+15*time.Second)
			legStart := time.Now()
			r := runOneCLI(legCtx, gw, e.cli, computeID, prompt, mode, model, *timeoutSec)
			legWall := int(time.Since(legStart).Round(time.Second).Seconds())
			cancelLeg()
			results <- result{
				idx:     i,
				cli:     e.cli,
				model:   model,
				compute: e.computeQuery,
				reply:   r.reply,
				usage:   r.usage,
				elapsed: r.elapsed,
				legWall: legWall,
				mode:    r.mode,
				err:     r.err,
			}
		}()
	}
	// Close the channel once all legs report, so the drain loop below can
	// range over it and update live status as each completes.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Drain into a stable order matching the user's pair sequence on
	// the command line. Index-based ordering also covers the duplicate-
	// CLI case where map-by-name would collapse rows. We print a live
	// completion line here, as results arrive (completion order).
	ordered := make([]result, len(entries))
	done := 0
	sumLegWall := 0
	for r := range results {
		ordered[r.idx] = r
		done++
		sumLegWall += r.legWall
		if r.err != nil {
			fmt.Printf("  ✗ %s — %v  (%d/%d)\n", legLabel(r.cli, r.model, r.compute), r.err, done, len(entries))
		} else {
			fmt.Printf("  ✓ %s — %ds  (%d/%d)\n", legLabel(r.cli, r.model, r.compute), r.legWall, done, len(entries))
		}
	}
	wallSec := int(time.Since(startAll).Round(time.Second).Seconds())
	fmt.Println()

	for _, r := range ordered {
		// Header takes the bare cli name (apiKeyEnvFor + Cost look it
		// up internally), so the compute label rides on a second line.
		if r.err != nil {
			fmt.Printf("==== %s (error) ====\n%s\n\n", legLabel(r.cli, r.model, r.compute), r.err)
			continue
		}
		fmt.Println(output.Header(r.cli, r.elapsed, r.mode, r.usage))
		if r.model != "" {
			fmt.Printf("  model %s · on %s\n", r.model, r.compute)
		} else {
			fmt.Printf("  on %s\n", r.compute)
		}
		fmt.Println(r.reply)
		fmt.Println()
	}
	// Make the saved time explicit: wall clock tracks the slowest leg, not
	// the sum — the whole point of running them concurrently.
	if len(entries) > 1 {
		fmt.Printf("— %d agents · %ds wall · ~%ds if run one-by-one (ran concurrently)\n",
			len(entries), wallSec, sumLegWall)
	} else {
		fmt.Printf("— %d agent · %ds wall\n", len(entries), wallSec)
	}
	return nil
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

// splitCLIModel parses a competitor token `harness[/model]`:
//
//	"claude/opus-4.8" → ("claude", "opus-4.8")
//	"claude"          → ("claude", "")
//
// Split on the first "/" only, so a model name that itself contains a
// slash (e.g. "anthropic/claude-…") survives intact.
func splitCLIModel(tok string) (cli, model string) {
	if i := strings.IndexByte(tok, '/'); i >= 0 {
		return strings.TrimSpace(tok[:i]), strings.TrimSpace(tok[i+1:])
	}
	return tok, ""
}

// legLabel renders a competitor for status lines: "claude/opus-4.8@mac-mini"
// when a model is pinned, "claude@mac-mini" otherwise.
func legLabel(cli, model, compute string) string {
	if model != "" {
		return fmt.Sprintf("%s/%s@%s", cli, model, compute)
	}
	return fmt.Sprintf("%s@%s", cli, compute)
}
