package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// TestSpec describes one cerver test — a prompt to run against a list
// of CLIs, plus simple per-response assertions for pass/fail signal.
// Tests are JSON files in ~/.cerver/tests/, one file per test, named
// <id>.json. Each run is archived under ~/.cerver/tests/runs/.
//
// Intentionally minimal: the unit is "does this prompt come back from
// every CLI within budget and look plausible". Richer assertions
// (semantic similarity, regex checks, structured-output parsing) can
// layer on later without changing the file shape.
type TestSpec struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Prompt     string       `json:"prompt"`
	CLIs       []string     `json:"clis,omitempty"`
	Compute    string       `json:"compute,omitempty"`
	MaxSeconds int          `json:"max_seconds,omitempty"`
	// Billing per CLI: `{"claude":"api","codex":"sub","grok":"api"}`.
	// Missing keys fall back to the vendor default (grok=api, others
	// subscription). Useful for isolating subscription-side throttling
	// (Anthropic / ChatGPT) by re-running the same prompt in api mode
	// where rate limits are predictable and paid per token.
	Billing map[string]string `json:"billing,omitempty"`
	Expect  ExpectClause      `json:"expect,omitempty"`

	// Internal — set when loaded from disk.
	path string `json:"-"`
}

// ExpectClause holds the per-response checks. AnyMentions is OR'd
// across the listed substrings (case-insensitive). MinChars enforces
// a minimum visible response length to catch silently-truncated runs.
type ExpectClause struct {
	MinChars    int      `json:"min_chars,omitempty"`
	AnyMentions []string `json:"any_mentions,omitempty"`
}

// TestResult is one CLI's result for one test.
type TestResult struct {
	CLI     string `json:"cli"`
	Reply   string `json:"reply"`
	Elapsed int    `json:"elapsed_seconds"`
	Mode    string `json:"mode"`
	Error   string `json:"error,omitempty"`
	Pass    bool   `json:"pass"`
	FailWhy string `json:"fail_why,omitempty"`
}

// TestRun is the on-disk archive for one execution of one test.
type TestRun struct {
	TestID    string       `json:"test_id"`
	TestName  string       `json:"test_name"`
	Prompt    string       `json:"prompt"`
	StartedAt time.Time    `json:"started_at"`
	Results   []TestResult `json:"results"`
	OverallOK bool         `json:"overall_ok"`
}

// Test is the `cerver test` entrypoint. Sub-shapes:
//
//	cerver test                   # list available tests
//	cerver test <id>              # run one test (id-prefix match)
//	cerver test --all             # run every test, sequentially
//	cerver test --list            # explicit list
//
// Tests live at ~/.cerver/tests/*.json. Runs are archived under
// ~/.cerver/tests/runs/<id>-<unix>.json so you can diff over time.
func Test(args []string) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	all := fs.Bool("all", false, "Run every test in ~/.cerver/tests/")
	listOnly := fs.Bool("list", false, "List tests, don't run anything")
	timeoutSec := fs.Int("timeout", 180, "Max seconds to wait for any single CLI reply")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := ensureTestsDir()
	if err != nil {
		return err
	}
	tests, err := loadTests(dir)
	if err != nil {
		return err
	}

	if *listOnly || (len(fs.Args()) == 0 && !*all) {
		printTestList(tests, dir)
		return nil
	}

	targets := tests
	if !*all {
		query := fs.Arg(0)
		picked := matchTests(tests, query)
		if len(picked) == 0 {
			return fmt.Errorf("no test matching %q (try `cerver test` to list)", query)
		}
		targets = picked
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*timeoutSec*len(targets)+30)*time.Second)
	defer cancel()

	cerverTok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if cerverTok == "" {
		return errors.New("no cerver credentials — run cerver.ai/install.sh first")
	}
	gw := gateway.New(cerverTok)

	// Resolve the compute once. Tests with an explicit `compute` field
	// override per-test; the default ("") picks the first ready local
	// relay just like `cerver run` / `cerver compare` do.
	allOK := true
	for i, t := range targets {
		if i > 0 {
			fmt.Println()
		}
		ok, err := runTest(ctx, gw, t, *timeoutSec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "test %s: %v\n", t.ID, err)
			allOK = false
			continue
		}
		if !ok {
			allOK = false
		}
	}
	if !allOK {
		os.Exit(1)
	}
	return nil
}

func ensureTestsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cerver", "tests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o755); err != nil {
		return "", err
	}
	// Seed starter tests on first use. Self-healing: any seed file
	// that doesn't exist gets created; existing files (and any user-
	// added ones) are never touched. This way adding a new seed in a
	// CLI release lands automatically without making users nuke their
	// tests directory.
	if err := writeSeedTest(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func writeSeedTest(dir string) error {
	// Seed two starter tests: the same prompt in subscription mode
	// (default) and in api-mode. Having both lets the user diagnose
	// flaky timeouts — if the subscription run fails but the api run
	// passes, the provider's subscription tier is throttling, not
	// cerver itself.
	prompt := "Design a rate-limiting strategy for a public API expecting bursts. " +
		"Pick ONE algorithm (token bucket, leaky bucket, or sliding window). " +
		"Justify in ~120 words with one concrete tradeoff you accept."
	expect := ExpectClause{
		MinChars:    200,
		AnyMentions: []string{"token bucket", "leaky bucket", "sliding window"},
	}
	// Seed pair: same prompt, one test per auth mode. Lets the user
	// confirm each path works independently and diagnose flakes by
	// comparing the two side-by-side.
	//
	// 01 = api mode for all three providers (proves vault keys + the
	//      api-mode CLI plumbing all work end-to-end). Costs real
	//      tokens — cheap (~$0.01 per run on this prompt) but real.
	//
	// 02 = subscription mode for claude + codex (proves the local
	//      OAuth path on each user's Max/Plus plan). Grok is omitted
	//      — it has no subscription path; cerver rejects grok=sub at
	//      session-create.
	seeds := []TestSpec{
		{
			ID:         "01_rate_limiter_api",
			Name:       "Rate limiter — API-key mode (all providers)",
			Prompt:     prompt,
			CLIs:       []string{"claude", "codex", "grok"},
			MaxSeconds: 90,
			Billing: map[string]string{
				"claude": "api",
				"codex":  "api",
				"grok":   "api",
			},
			Expect: expect,
		},
		{
			ID:         "02_rate_limiter_subscription",
			Name:       "Rate limiter — subscription mode (claude + codex only)",
			Prompt:     prompt,
			CLIs:       []string{"claude", "codex"},
			MaxSeconds: 90,
			Billing: map[string]string{
				"claude": "subscription",
				"codex":  "subscription",
			},
			Expect: expect,
		},
	}
	for _, t := range seeds {
		target := filepath.Join(dir, t.ID+".json")
		if _, err := os.Stat(target); err == nil {
			// Already there — never overwrite a user's edits.
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		body, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return err
		}
		body = append(body, '\n')
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func loadTests(dir string) ([]TestSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tests []TestSpec
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var t TestSpec
		if err := json.Unmarshal(body, &t); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		// If the file forgot to set ID, infer from filename.
		if t.ID == "" {
			t.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		t.path = full
		tests = append(tests, t)
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
	return tests, nil
}

func matchTests(tests []TestSpec, query string) []TestSpec {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	var out []TestSpec
	for _, t := range tests {
		if t.ID == query || strings.HasPrefix(t.ID, query) {
			out = append(out, t)
		}
	}
	return out
}

func printTestList(tests []TestSpec, dir string) {
	fmt.Printf("Tests directory: %s\n\n", dir)
	if len(tests) == 0 {
		fmt.Println("(no tests yet — drop a JSON file here and re-run)")
		return
	}
	fmt.Printf("%-22s  %s\n", "ID", "NAME")
	fmt.Printf("%-22s  %s\n", strings.Repeat("-", 22), strings.Repeat("-", 40))
	for _, t := range tests {
		fmt.Printf("%-22s  %s\n", t.ID, t.Name)
	}
	fmt.Println()
	fmt.Println("Run one:   cerver test <id>")
	fmt.Println("Run all:   cerver test --all")
}

func runTest(ctx context.Context, gw *gateway.Client, t TestSpec, defaultTimeoutSec int) (bool, error) {
	clis := t.CLIs
	if len(clis) == 0 {
		clis = []string{"claude", "codex", "grok"}
	}
	timeoutSec := t.MaxSeconds
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}

	computeID, err := pickCompute(ctx, gw, t.Compute)
	if err != nil {
		return false, fmt.Errorf("compute: %w", err)
	}

	// Resolve per-CLI billing mode. Default: grok=api, others=sub.
	// The test's `billing` field can override per CLI — e.g. a copy
	// of the same test in api-mode to isolate subscription throttling.
	billingMode := func(cli string) string {
		if m, ok := t.Billing[cli]; ok {
			switch strings.ToLower(strings.TrimSpace(m)) {
			case "api":
				return "api"
			case "sub", "subscription":
				return "subscription"
			}
		}
		if cli == "grok" {
			return "api"
		}
		return "subscription"
	}

	// Lazy Infisical — needed for any CLI in api mode (XAI_API_KEY
	// for grok, ANTHROPIC_API_KEY for claude, OPENAI_API_KEY for
	// codex), so initialize whenever the per-CLI map has any api.
	var inf *infisical.Client
	needsInfisical := false
	for _, c := range clis {
		if billingMode(c) == "api" {
			needsInfisical = true
			break
		}
	}
	if needsInfisical {
		icfg, err := infisical.LoadConfig()
		if err != nil {
			return false, fmt.Errorf("api-mode test needs Infisical for vendor keys: %w", err)
		}
		inf = infisical.New(icfg)
	}

	printTestHeader(t, clis, computeID, timeoutSec)

	// Preflight every CLI in parallel — auth check + provider-API
	// reachability. A failed preflight short-circuits the test for
	// that CLI (no 90s wait for a CLI we already know can't run).
	// Preflight always benefits from Infisical (for grok's
	// XAI_API_KEY auth check) — load it if we haven't already.
	if inf == nil {
		if icfg, err := infisical.LoadConfig(); err == nil {
			inf = infisical.New(icfg)
		}
	}
	// Preflight runs both checks concurrently per CLI, but we render
	// them in two separate phase blocks so the user can see at a
	// glance which one passed / failed. Health = "can we reach the
	// provider's server?" — purely a network probe. Auth = "are we
	// signed in / do we have an API key?" — independent of network.
	preflights := make(map[string]PreflightResult, len(clis))
	{
		type slot struct {
			cli string
			pf  PreflightResult
		}
		ch := make(chan slot, len(clis))
		var pwg sync.WaitGroup
		for _, c := range clis {
			c := c
			pwg.Add(1)
			go func() {
				defer pwg.Done()
				ch <- slot{cli: c, pf: preflightCheck(ctx, c, inf)}
			}()
		}
		pwg.Wait()
		close(ch)
		for s := range ch {
			preflights[s.cli] = s.pf
		}
	}
	printPhaseHeader("Health")
	for _, c := range clis {
		printHealthRow(preflights[c])
	}
	fmt.Println()
	printPhaseHeader("Auth")
	for _, c := range clis {
		printAuthRow(preflights[c])
	}
	fmt.Println()
	printPhaseHeader("Running")

	type slot struct {
		idx int
		cli string
		res TestResult
	}
	results := make(chan slot, len(clis))
	// Track which CLIs are still in flight so the heartbeat can show
	// "waiting on claude, grok" instead of just a spinner. Updated by
	// goroutines under remainingMu; read by the heartbeat ticker.
	var remainingMu sync.Mutex
	remaining := make(map[string]bool, len(clis))
	for _, c := range clis {
		remaining[c] = true
	}
	var wg sync.WaitGroup
	for i, cli := range clis {
		i, cli := i, cli
		mode := billingMode(cli)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Short-circuit on failed preflight — saves the 90s
			// timeout when we already know the CLI can't run.
			if pf := preflights[cli]; !pf.Pass() {
				reason := pf.AuthDetail
				if !pf.HealthOK {
					reason = pf.HealthDetail
				}
				res := TestResult{
					CLI: cli, Mode: mode,
					Error:   "preflight failed: " + reason,
					Pass:    false,
					FailWhy: "preflight failed",
				}
				remainingMu.Lock()
				delete(remaining, cli)
				remainingMu.Unlock()
				printDoneLine(cli, 0, "skipped")
				results <- slot{idx: i, cli: cli, res: res}
				return
			}
			printSpawnLine(cli, mode)
			r := runOneCLI(ctx, inf, gw, cli, computeID, t.Prompt, mode, "", timeoutSec)
			res := TestResult{CLI: cli, Mode: r.mode, Elapsed: r.elapsed}
			if r.err != nil {
				res.Error = r.err.Error()
			} else {
				res.Reply = r.reply
			}
			res.Pass, res.FailWhy = evalExpect(res, t.Expect)
			remainingMu.Lock()
			delete(remaining, cli)
			remainingMu.Unlock()
			tag := "ok"
			if res.Error != "" {
				tag = "ERR"
			} else if !res.Pass {
				tag = "FAIL"
			}
			printDoneLine(cli, res.Elapsed, tag)
			results <- slot{idx: i, cli: cli, res: res}
		}()
	}

	// Heartbeat: every 10s while goroutines run, print what we're
	// still waiting on. Stops as soon as wg.Wait() returns.
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ticker.C:
				remainingMu.Lock()
				if len(remaining) == 0 {
					remainingMu.Unlock()
					return
				}
				names := make([]string, 0, len(remaining))
				for c := range remaining {
					names = append(names, c)
				}
				remainingMu.Unlock()
				sort.Strings(names)
				printWaitingLine(names)
			}
		}
	}()
	wg.Wait()
	close(heartbeatDone)
	close(results)
	fmt.Println()

	ordered := make([]TestResult, len(clis))
	for s := range results {
		ordered[s.idx] = s.res
	}

	fmt.Println()
	printPhaseHeader("Results")
	fmt.Println()
	// Compact table first — pass/fail + timings at a glance — then
	// full response bodies for the cases worth reading. Failures
	// surface in the table with a fail-reason follow-up line.
	printResultTable(preflights, ordered)
	overallOK := true
	passed := 0
	for _, r := range ordered {
		if r.Pass {
			passed++
		} else {
			overallOK = false
		}
		printResultPanel(r, 64)
	}

	if err := archiveRun(t, ordered, overallOK); err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
	}

	printSummary(t.ID, passed, len(ordered))
	return overallOK, nil
}

// evalExpect applies the test's pass/fail rules to one CLI's response.
// Errors propagate as automatic fails. MinChars and AnyMentions stack
// (AND) — must satisfy both.
func evalExpect(r TestResult, ec ExpectClause) (bool, string) {
	if r.Error != "" {
		return false, "cli error"
	}
	if ec.MinChars > 0 && len(r.Reply) < ec.MinChars {
		return false, fmt.Sprintf("reply %d chars < min %d", len(r.Reply), ec.MinChars)
	}
	if len(ec.AnyMentions) > 0 {
		lower := strings.ToLower(r.Reply)
		hit := false
		for _, m := range ec.AnyMentions {
			if strings.Contains(lower, strings.ToLower(m)) {
				hit = true
				break
			}
		}
		if !hit {
			return false, fmt.Sprintf("none of %v in reply", ec.AnyMentions)
		}
	}
	return true, ""
}

func archiveRun(t TestSpec, results []TestResult, ok bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	runsDir := filepath.Join(home, ".cerver", "tests", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return err
	}
	run := TestRun{
		TestID:    t.ID,
		TestName:  t.Name,
		Prompt:    t.Prompt,
		StartedAt: time.Now().UTC(),
		Results:   results,
		OverallOK: ok,
	}
	body, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	fn := fmt.Sprintf("%s-%d.json", t.ID, time.Now().Unix())
	return os.WriteFile(filepath.Join(runsDir, fn), body, 0o644)
}
