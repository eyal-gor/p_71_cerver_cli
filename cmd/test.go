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
	"github.com/eyal-gor/p_71_cerver_cli/internal/output"
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
	Expect     ExpectClause `json:"expect,omitempty"`

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
	// Seed a starter test on first use. Skips silently if any *.json
	// already exists (so deletions don't reseed every run).
	entries, _ := os.ReadDir(dir)
	hasTest := false
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			hasTest = true
			break
		}
	}
	if !hasTest {
		if err := writeSeedTest(dir); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func writeSeedTest(dir string) error {
	t := TestSpec{
		ID:   "01_rate_limiter",
		Name: "Rate limiter algorithm choice",
		Prompt: "Design a rate-limiting strategy for a public API expecting bursts. " +
			"Pick ONE algorithm (token bucket, leaky bucket, or sliding window). " +
			"Justify in ~120 words with one concrete tradeoff you accept.",
		CLIs:       []string{"claude", "codex", "grok"},
		MaxSeconds: 90,
		Expect: ExpectClause{
			MinChars:    200,
			AnyMentions: []string{"token bucket", "leaky bucket", "sliding window"},
		},
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(filepath.Join(dir, t.ID+".json"), body, 0o644)
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

	// Lazy Infisical for grok (always api mode). Initialize only if
	// needed so subscription-only tests don't pay the load cost.
	var inf *infisical.Client
	for _, c := range clis {
		if c == "grok" {
			icfg, err := infisical.LoadConfig()
			if err != nil {
				return false, fmt.Errorf("grok needs Infisical for XAI_API_KEY: %w", err)
			}
			inf = infisical.New(icfg)
			break
		}
	}

	fmt.Printf("==== test %s · %s ====\n", t.ID, t.Name)
	fmt.Printf("clis: %s · compute: %s · timeout: %ds\n\n", strings.Join(clis, ","), computeID, timeoutSec)

	type slot struct {
		idx int
		cli string
		res TestResult
	}
	results := make(chan slot, len(clis))
	var wg sync.WaitGroup
	for i, cli := range clis {
		i, cli := i, cli
		mode := "subscription"
		if cli == "grok" {
			mode = "api"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := runOneCLI(ctx, inf, gw, cli, computeID, t.Prompt, mode, "", timeoutSec)
			res := TestResult{CLI: cli, Mode: r.mode, Elapsed: r.elapsed}
			if r.err != nil {
				res.Error = r.err.Error()
			} else {
				res.Reply = r.reply
			}
			res.Pass, res.FailWhy = evalExpect(res, t.Expect)
			results <- slot{idx: i, cli: cli, res: res}
		}()
	}
	wg.Wait()
	close(results)

	ordered := make([]TestResult, len(clis))
	for s := range results {
		ordered[s.idx] = s.res
	}

	overallOK := true
	for _, r := range ordered {
		usage := (*gateway.Usage)(nil) // archive shape, no per-run usage yet
		fmt.Println(output.Header(r.CLI, r.Elapsed, r.Mode, usage))
		if r.Error != "" {
			fmt.Printf("  ERROR: %s\n", r.Error)
		} else {
			fmt.Println(r.Reply)
		}
		status := "PASS"
		attr := ""
		if !r.Pass {
			status = "FAIL"
			attr = " — " + r.FailWhy
			overallOK = false
		}
		fmt.Printf("  → %s%s\n\n", status, attr)
	}

	if err := archiveRun(t, ordered, overallOK); err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
	}

	overall := "PASS"
	if !overallOK {
		overall = "FAIL"
	}
	fmt.Printf("test %s overall: %s\n", t.ID, overall)
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
