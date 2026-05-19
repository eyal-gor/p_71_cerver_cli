package cmd

import (
	"fmt"
	"os"
	"strings"
)

// Visual elements for `cerver test` output. Lives separately from the
// run-logic in test.go so the print/format concerns are easy to swap
// or theme without touching session-spawning logic.
//
// Design priorities:
//   1. Phase-aware: the user can see at a glance whether the run is
//      in preflight, running, or reporting results.
//   2. Aligned columns: CLI names, status icons, times all line up so
//      a triple-CLI test reads like a small table, not a paragraph.
//   3. TTY-aware: ANSI colors only emit when stdout is an interactive
//      terminal — piping to a file or `tee` stays clean text.

// useANSI returns true when stdout is a tty (so colors / cursor codes
// won't corrupt a log file or `tee` output). Detect via os.Stdout
// stat — stdlib only, no x/term dependency.
var useANSI = func() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	st, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (st.Mode() & os.ModeCharDevice) != 0
}()

func ansi(code, s string) string {
	if !useANSI {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string  { return ansi("1", s) }
func dim(s string) string   { return ansi("2", s) }
func green(s string) string { return ansi("32", s) }
func red(s string) string   { return ansi("31", s) }
func cyan(s string) string  { return ansi("36", s) }

// box characters — fall back to ASCII when ANSI is off, on the
// assumption that no-ANSI also means a non-UTF8 capture target.
var (
	boxH        = "─"
	boxHThick   = "━"
	chevron     = "▸"
	dotMid      = "·"
	checkMark   = "✓"
	crossMark   = "✗"
	arrowRight  = "→"
	arrowLeft   = "←"
	pendingMark = "·"
)

func init() {
	if !useANSI {
		// Box-drawing chars require UTF-8; downgrade if we suspect
		// the pipe target can't handle them.
		boxH = "-"
		boxHThick = "="
		chevron = ">"
		dotMid = "·" // keep — UTF-8 middle dot is fine in most pipes
		checkMark = "PASS"
		crossMark = "FAIL"
		arrowRight = "->"
		arrowLeft = "<-"
		pendingMark = "."
	}
}

// printTestHeader renders the top-of-run frame with the test's id,
// name, configuration, and prompt. Prompt wraps to bar_w with a
// hanging indent so multi-line prompts read as a paragraph rather
// than a wall of text.
func printTestHeader(t TestSpec, clis []string, computeID string, timeoutSec int) {
	width := 64
	bar := strings.Repeat(boxHThick, width)
	fmt.Println(bold(bar))
	fmt.Printf(" %s  %s\n", bold("test "+t.ID), dim(t.Name))
	fmt.Println(dim(strings.Repeat(boxH, width)))
	fmt.Printf(" %-10s %s\n", dim("CLIs"), strings.Join(clis, "  "+dotMid+"  "))
	fmt.Printf(" %-10s %s\n", dim("Compute"), computeID)
	fmt.Printf(" %-10s %ds\n", dim("Timeout"), timeoutSec)
	fmt.Printf(" %-10s\n", dim("Prompt"))
	for _, line := range wrap(strings.TrimSpace(t.Prompt), width-4) {
		fmt.Printf("   %s\n", line)
	}
	fmt.Println(bold(bar))
	fmt.Println()
}

// printPhaseHeader prints a phase marker (preflight / running / results).
func printPhaseHeader(name string) {
	fmt.Printf("%s %s\n", cyan(chevron), bold(name))
}

// printPreflightRow renders one CLI's preflight summary in aligned
// columns. Long auth detail strings are truncated so the columns
// don't break across CLIs with different-length auth labels.
func printPreflightRow(pf PreflightResult) {
	icon := green(checkMark)
	if !pf.Pass() {
		icon = red(crossMark)
	}
	auth := truncFit(pf.AuthDetail, 44)
	health := pf.HealthDetail
	fmt.Printf("  %s  %-7s  auth: %-44s  health: %s\n",
		icon, bold(pf.CLI), auth, health)
}

// printSpawnLine — emitted as each CLI starts in the "Running" phase.
func printSpawnLine(cli, mode string) {
	fmt.Printf("  %s  %-7s %s\n", cyan(arrowRight), bold(cli), dim("("+mode+")"))
}

// printDoneLine — emitted when a CLI finishes. tag is "ok"/"FAIL"/"ERR".
func printDoneLine(cli string, elapsed int, tag string) {
	icon := green(checkMark)
	if tag == "FAIL" || tag == "ERR" {
		icon = red(crossMark)
	}
	fmt.Printf("  %s  %-7s done in %ds  %s\n", icon, bold(cli), elapsed, dim("("+tag+")"))
}

// printWaitingLine — heartbeat output every N seconds during the run.
func printWaitingLine(names []string) {
	fmt.Printf("  %s  waiting on: %s\n", dim(pendingMark), dim(strings.Join(names, ", ")))
}

// printResultTable — compact at-a-glance summary of every CLI's
// auth, health, runtime, and verdict. Renders before the response
// bodies so the user sees pass/fail status without scrolling past
// long responses first.
//
// Layout (UTF-8 box-drawing in TTY mode, ASCII fallback otherwise):
//
//   ┌─────────┬──────────────┬──────┬──────────┬──────┬──────────┐
//   │ CLI     │ Mode         │ Auth │ Health   │ Time │ Verdict  │
//   ├─────────┼──────────────┼──────┼──────────┼──────┼──────────┤
//   │ claude  │ subscription │  ✓   │ 200·80ms │ 21s  │ ✓ PASS   │
//   │ codex   │ subscription │  ✓   │ 421·35ms │ 20s  │ ✓ PASS   │
//   │ grok    │ api          │  ✓   │ 421·184ms│ 21s  │ ✓ PASS   │
//   └─────────┴──────────────┴──────┴──────────┴──────┴──────────┘
//
// Failure rows show a follow-up line with the fail reason. Healthy
// rows stay one-line.
func printResultTable(prefs map[string]PreflightResult, results []TestResult) {
	// Pick characters based on ANSI/UTF-8 availability.
	var (
		tl, tr, bl, br, mh, mv, mc string
	)
	if useANSI {
		tl, tr, bl, br = "┌", "┐", "└", "┘"
		mh, mv = "─", "│"
		mc = "┼"
	} else {
		tl, tr, bl, br = "+", "+", "+", "+"
		mh, mv = "-", "|"
		mc = "+"
	}
	// Column widths chosen so claude/codex/grok and subscription/api
	// fit naturally without truncation. Health column sized for the
	// "HTTP <code> · <ms>ms" format.
	w := []int{8, 14, 6, 12, 6, 10}
	headers := []string{"CLI", "Mode", "Auth", "Health", "Time", "Verdict"}

	makeRow := func(parts []string, color func(string) string) string {
		cells := make([]string, len(parts))
		for i, p := range parts {
			cells[i] = " " + padRight(p, w[i]-1)
		}
		row := mv + strings.Join(cells, mv) + mv
		if color != nil {
			return color(row)
		}
		return row
	}
	border := func(left, mid, right string) string {
		segs := make([]string, len(w))
		for i, ww := range w {
			segs[i] = strings.Repeat(mh, ww)
		}
		return left + strings.Join(segs, mid) + right
	}

	// Top + header + separator
	fmt.Println(dim(border(tl, intersectionDownTop(useANSI), tr)))
	fmt.Println(makeRow(headers, bold))
	fmt.Println(dim(border(intersectionLeft(useANSI), mc, intersectionRight(useANSI))))

	// Body rows
	for _, r := range results {
		pf := prefs[r.CLI]
		authIcon := green(checkMark)
		if !pf.AuthOK {
			authIcon = red(crossMark)
		}
		healthCell := "—"
		if pf.HealthDetail != "" {
			// Compact: strip "HTTP " prefix and " · " spacing to fit
			h := strings.TrimPrefix(pf.HealthDetail, "HTTP ")
			h = strings.ReplaceAll(h, " · ", "·")
			healthCell = truncFit(h, w[3]-2)
		}
		timeCell := fmt.Sprintf("%ds", r.Elapsed)
		verdict := green(checkMark + " PASS")
		if !r.Pass {
			verdict = red(crossMark + " FAIL")
		}
		fmt.Println(makeRow([]string{
			r.CLI,
			r.Mode,
			" " + authIcon,
			healthCell,
			timeCell,
			verdict,
		}, nil))
		// Show the fail reason as an indented follow-up under the row
		// when there is one. Keeps the table grid clean while still
		// surfacing actionable detail.
		if !r.Pass && r.FailWhy != "" {
			fmt.Printf("  %s %s: %s\n", red(crossMark), bold(r.CLI), dim(r.FailWhy))
		}
	}
	fmt.Println(dim(border(bl, intersectionUpBottom(useANSI), br)))
	fmt.Println()
}

// padRight pads a string with spaces to a fixed visible width. ANSI
// codes are zero-width visually but inflate len(s) — so the function
// computes the un-escaped length and pads accordingly. Crude (only
// strips well-formed CSI sequences) but enough for our color uses.
func padRight(s string, width int) string {
	visible := 0
	inEscape := false
	for _, r := range s {
		if r == 0x1b {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		visible++
	}
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

// Box-drawing intersection characters — tee shapes for the table's
// top/bottom row borders meeting the header separator. Helpers wrap
// the lookup so we can fall back to "+" in ASCII mode without ifs
// peppered throughout the table rendering.
func intersectionDownTop(ansi bool) string {
	if ansi {
		return "┬"
	}
	return "+"
}
func intersectionUpBottom(ansi bool) string {
	if ansi {
		return "┴"
	}
	return "+"
}
func intersectionLeft(ansi bool) string {
	if ansi {
		return "├"
	}
	return "+"
}
func intersectionRight(ansi bool) string {
	if ansi {
		return "┤"
	}
	return "+"
}

// printResultPanel — the per-CLI output block. Bar at top with the
// summary line; reply body; bar at bottom carrying the verdict.
func printResultPanel(r TestResult, width int) {
	if width <= 0 {
		width = 64
	}
	headerInfo := fmt.Sprintf("%s %s %s %s %s", bold(r.CLI), dim(dotMid), fmt.Sprintf("%ds", r.Elapsed), dim(dotMid), r.Mode)
	fmt.Println(dim(strings.Repeat(boxH, width)))
	fmt.Printf(" %s\n", headerInfo)
	fmt.Println(dim(strings.Repeat(boxH, width)))
	if r.Error != "" {
		fmt.Printf(" %s %s\n", red(crossMark), r.Error)
	} else {
		fmt.Println(r.Reply)
	}
	verdict := green(checkMark + " PASS")
	if !r.Pass {
		verdict = red(crossMark + " FAIL")
		if r.FailWhy != "" {
			verdict += dim("  ("+r.FailWhy+")")
		}
	}
	fmt.Println(dim(strings.Repeat(boxH, width)))
	fmt.Printf(" %s\n", verdict)
	fmt.Println()
}

// printSummary — final verdict bar at the bottom of one test run.
func printSummary(testID string, passed, total int) {
	width := 64
	bar := strings.Repeat(boxHThick, width)
	verdict := green(checkMark + " PASS")
	if passed < total {
		verdict = red(crossMark + " FAIL")
	}
	fmt.Println(bold(bar))
	fmt.Printf(" %s   %s   %s\n",
		bold("test "+testID),
		verdict,
		dim(fmt.Sprintf("%d/%d CLIs passed", passed, total)))
	fmt.Println(bold(bar))
}

// wrap splits text into lines of ≤ width characters at word boundaries.
// Keeps existing newlines as paragraph breaks. Stdlib-only, no deps.
func wrap(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) > width {
				out = append(out, line)
				line = w
			} else {
				line += " " + w
			}
		}
		out = append(out, line)
	}
	return out
}

// truncFit is the cmd-package local helper — distinct from the
// existing truncate() over in sessions.go which trims display
// labels in the sessions table (different ellipsis style).
func truncFit(s string, max int) string {
	if max <= 1 {
		return s
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
