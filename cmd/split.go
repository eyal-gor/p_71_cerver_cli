package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Split hands one task to cerver's orchestrator: a frontier planner breaks it
// into subtasks, each subtask runs on the cheapest capable model (the account
// routing policy clamps the planner's tier picks), and a merge step assembles
// the single final answer. Prints live progress from the SSE stream, the
// answer, and the cost next to what frontier-only would have cost.
//
// Usage:
//
//	cerver split "write a landing page with pricing and FAQ"
//	cerver split --demo "same task"     # ALSO runs the task monolithically on
//	                                    # the frontier model, has a blind judge
//	                                    # score both answers, and prints a
//	                                    # shareable /c/<id> comparison link
//	cerver split --json "task"          # raw SSE events, one JSON per line
func Split(args []string) error {
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	demo := fs.Bool("demo", false, "Also run the task whole on the frontier model and publish a cost/quality comparison")
	asJSON := fs.Bool("json", false, "Emit raw orchestrate events as JSON lines")
	timeoutMin := fs.Int("timeout", 15, "Max minutes to wait for the run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return errors.New(`usage: cerver split [--demo] [--json] "<task>"`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()

	tok, err := infisical.LoadRunToken(ctx)
	if err != nil {
		return fmt.Errorf("no cerver credentials (run `cerver login` first): %w", err)
	}
	gw := gateway.New(tok)
	if v := os.Getenv("CERVER_GATEWAY_URL"); v != "" {
		gw.BaseURL = strings.TrimRight(v, "/")
	}

	if !*asJSON {
		fmt.Printf("→ splitting: %s\n", task)
	}
	exitErr := error(nil)
	sawDone := false
	err = gw.SplitStream(ctx, task, *demo, func(ev gateway.SplitEvent) {
		if *asJSON {
			if b, err := json.Marshal(ev); err == nil {
				fmt.Println(string(b))
			}
			return
		}
		switch ev.Type {
		case "plan":
			fmt.Printf("\nplan (%d subtask%s):\n", len(ev.Plan), plural(len(ev.Plan)))
			for _, p := range ev.Plan {
				model := p.Model
				if p.Verify {
					model = "verify ×2 + judge"
				}
				deps := ""
				if len(p.DependsOn) > 0 {
					deps = fmt.Sprintf(" (after #%s)", joinInts(p.DependsOn, ", #"))
				}
				fmt.Printf("  #%d  %-8s %-28s %s%s\n", p.ID, p.Tier, model, p.Desc, deps)
			}
			fmt.Println()
		case "step":
			if ev.Step != nil {
				fmt.Printf("✓ #%d %s · %s\n", ev.Step.ID, shortModel(ev.Step.Model), trunc(ev.Step.Desc, 60))
			}
		case "step_failed":
			fmt.Printf("✗ #%d %s — %s\n", ev.ID, trunc(ev.Desc, 50), trunc(ev.Error, 120))
		case "baseline_done":
			fmt.Printf("⚡ frontier baseline done (%s, %.1fs)\n", shortModel(ev.Model), ev.LatencyMs/1000)
		case "merge":
			fmt.Printf("… merging on %s\n", shortModel(ev.Model))
		case "needs_resolution":
			fmt.Printf("\n⏸ paused: subtask #%d needs a human call — two models disagreed.\n", ev.SubtaskID)
			fmt.Printf("  resolve in the dashboard (Runs → %s) or:\n", ev.RunID)
			fmt.Printf("  curl -X POST %s/v2/orchestrate/%s/resolve -d '{\"choice\":\"a|b|tiebreaker\"}'\n", gw.BaseURL, ev.RunID)
			exitErr = errors.New("run paused at needs_resolution")
		case "error":
			exitErr = fmt.Errorf("orchestrate failed: %s", ev.Message)
		case "done":
			sawDone = true
			if ev.Final != "" {
				fmt.Printf("\n── answer ──\n%s\n\n", ev.Final)
			}
			cost := fmt.Sprintf("cost $%.4f", ev.CostUsd)
			switch {
			case *demo && ev.FrontierCostUsd > 0:
				fmt.Printf("%s · frontier ran the same task: $%.4f%s\n", cost, ev.FrontierCostUsd, savedPct(ev.CostUsd, ev.FrontierCostUsd))
				if v := ev.DoneVerdict(); v != nil {
					fmt.Printf("judge: %s — %s\n", v.Winner, v.Reasoning)
				}
				if ev.ShareURL != "" {
					fmt.Printf("share: %s\n", ev.ShareURL)
				}
			case ev.FrontierEstUsd > 0:
				fmt.Printf("%s · frontier-only would be ~$%.4f%s\n", cost, ev.FrontierEstUsd, savedPct(ev.CostUsd, ev.FrontierEstUsd))
			default:
				fmt.Println(cost)
			}
			if ev.Status == "partially_done" {
				exitErr = errors.New("run finished partially_done — some subtasks failed (see ✗ lines above)")
			}
		}
	})
	if err != nil {
		return err
	}
	if exitErr != nil {
		return exitErr
	}
	if !sawDone && !*asJSON {
		return errors.New("stream ended without a done event (run may still be in the dashboard Runs page)")
	}
	return nil
}

func savedPct(cost, frontier float64) string {
	if frontier <= 0 || cost >= frontier {
		return ""
	}
	return fmt.Sprintf(" (%.0f%% saved)", (1-cost/frontier)*100)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func joinInts(xs []int, sep string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, sep)
}

func shortModel(m string) string {
	m = strings.TrimSpace(m)
	// claude-haiku-4-5-20251001 → claude-haiku-4-5 (drop a trailing date stamp)
	if i := strings.LastIndex(m, "-"); i > 0 && len(m)-i-1 == 8 && allDigits(m[i+1:]) {
		m = m[:i]
	}
	return m
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
