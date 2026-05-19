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

func bold(s string) string   { return ansi("1", s) }
func dim(s string) string    { return ansi("2", s) }
func green(s string) string  { return ansi("32", s) }
func red(s string) string    { return ansi("31", s) }
func cyan(s string) string   { return ansi("36", s) }
func yellow(s string) string { return ansi("33", s) }

// box characters — fall back to ASCII when ANSI is off, on the
// assumption that no-ANSI also means a non-UTF8 capture target.
var (
	boxH        = "─"
	boxHThick   = "━"
	chevron     = "▸"
	dotMid      = "·"
	checkMark   = "✓"
	crossMark   = "✗"
	warnMark    = "⚠"
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
		// Single-char marks compose with "PASS"/"FAIL"/"OVERLOAD" labels
		// without doubling up ("FAIL FAIL"). Stay short so column widths
		// match the UTF-8 path within ±1 char.
		checkMark = "+"
		crossMark = "x"
		warnMark = "!"
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

// printHealthRow renders one CLI's health-check result. Health is
// "can we reach the provider's API server" — purely a network probe,
// independent of whether the user is signed in.
func printHealthRow(pf PreflightResult) {
	icon := green(checkMark)
	if !pf.HealthOK {
		icon = red(crossMark)
	}
	fmt.Printf("  %s  %-7s  %s\n", icon, bold(pf.CLI), pf.HealthDetail)
}

// printAuthRow renders one CLI's auth-check result. Auth is "are we
// signed in / do we have a usable API key" — independent of network
// state. Long auth strings get truncated so the column stays aligned.
func printAuthRow(pf PreflightResult) {
	icon := green(checkMark)
	if !pf.AuthOK {
		icon = red(crossMark)
	}
	fmt.Printf("  %s  %-7s  %s\n", icon, bold(pf.CLI), truncFit(pf.AuthDetail, 60))
}

// printSpawnLine — emitted as each CLI starts in the "Running" phase.
func printSpawnLine(cli, mode string) {
	fmt.Printf("  %s  %-7s %s\n", cyan(arrowRight), bold(cli), dim("("+mode+")"))
}

// printDoneLine — emitted when a CLI finishes. tag is one of
// "ok" / "FAIL" / "ERR" / "OVERLOAD". OVERLOAD is shown in yellow so
// it visually distinguishes from a red FAIL — a provider 529 isn't a
// real failure and shouldn't make the run feel broken.
func printDoneLine(cli string, elapsed int, tag string) {
	var icon string
	switch tag {
	case "ok":
		icon = green(checkMark)
	case "OVERLOAD":
		icon = yellow(warnMark)
	default: // FAIL, ERR
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

	// Body rows — first pass renders the table itself. Fail / overload
	// reason lines are collected and printed AFTER the bottom border
	// so they don't interrupt the grid. Earlier version printed them
	// between rows and broke the table visually.
	type note struct {
		cli, why string
		kind     string // "fail" or "overload"
	}
	var notes []note
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
		var verdict string
		switch {
		case r.Pass:
			verdict = green(checkMark + " PASS")
		case r.Transient:
			// Yellow OVERLOAD reads as "not your fault, try again"
			// rather than the harsh red FAIL.
			verdict = yellow(warnMark + " OVERLD")
		default:
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
		if !r.Pass && r.FailWhy != "" {
			kind := "fail"
			if r.Transient {
				kind = "overload"
			}
			notes = append(notes, note{cli: r.CLI, why: r.FailWhy, kind: kind})
		}
	}
	fmt.Println(dim(border(bl, intersectionUpBottom(useANSI), br)))
	// Reason follow-ups go AFTER the table closes — keeps the grid
	// intact while still surfacing why a row failed or got throttled.
	for _, n := range notes {
		icon := red(crossMark)
		if n.kind == "overload" {
			icon = yellow(warnMark)
		}
		fmt.Printf("  %s %s: %s\n", icon, bold(n.cli), dim(n.why))
	}
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
	var verdict string
	switch {
	case r.Pass:
		verdict = green(checkMark + " PASS")
	case r.Transient:
		verdict = yellow(warnMark + " OVERLOAD")
		if r.FailWhy != "" {
			verdict += dim("  (" + r.FailWhy + ")")
		}
	default:
		verdict = red(crossMark + " FAIL")
		if r.FailWhy != "" {
			verdict += dim("  (" + r.FailWhy + ")")
		}
	}
	fmt.Println(dim(strings.Repeat(boxH, width)))
	fmt.Printf(" %s\n", verdict)
	fmt.Println()
}

// printSummary — final verdict bar at the bottom of one test run.
// Overloads (provider 529) are reported but don't fail the suite —
// the test ran, the prompt was valid, the provider was just busy.
func printSummary(testID string, passed, transient, total int) {
	width := 64
	bar := strings.Repeat(boxHThick, width)
	hardFail := total - passed - transient
	var verdict string
	switch {
	case hardFail > 0:
		verdict = red(crossMark + " FAIL")
	case transient > 0:
		verdict = yellow(warnMark + " PASS (some overloaded — retry)")
	default:
		verdict = green(checkMark + " PASS")
	}
	detail := fmt.Sprintf("%d/%d CLIs passed", passed, total)
	if transient > 0 {
		detail += fmt.Sprintf(", %d overloaded", transient)
	}
	fmt.Println(bold(bar))
	fmt.Printf(" %s   %s   %s\n",
		bold("test "+testID),
		verdict,
		dim(detail))
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

// printSwitchTestHeader — top-of-run frame for multi-step CLI-switch
// tests. Mirrors printTestHeader's visual idiom but lists the planned
// CLI sequence ("claude → codex → claude") instead of the parallel set.
func printSwitchTestHeader(t TestSpec, computeID string, timeoutSec int) {
	width := 64
	bar := strings.Repeat(boxHThick, width)
	fmt.Println(bold(bar))
	fmt.Printf(" %s  %s\n", bold("test "+t.ID), dim(t.Name))
	fmt.Println(dim(strings.Repeat(boxH, width)))
	seq := make([]string, len(t.Steps))
	for i, s := range t.Steps {
		seq[i] = s.CLI
	}
	fmt.Printf(" %-10s %s\n", dim("Sequence"), strings.Join(seq, "  "+arrowRight+"  "))
	fmt.Printf(" %-10s %s\n", dim("Compute"), computeID)
	fmt.Printf(" %-10s %ds\n", dim("Timeout"), timeoutSec)
	fmt.Printf(" %-10s %d\n", dim("Steps"), len(t.Steps))
	fmt.Println(bold(bar))
	fmt.Println()
}

// printSwitchStepHeader — per-turn banner. Shows the position
// (step 2/3), the CLI for this turn, an optional label from the spec,
// and the prompt wrapped for readability.
func printSwitchStepHeader(idx, total int, step TestStep, mode string) {
	printPhaseHeader(fmt.Sprintf("Step %d/%d — %s (%s)", idx+1, total, step.CLI, mode))
	if step.Label != "" {
		fmt.Printf("  %s %s\n", dim(dotMid), dim(step.Label))
	}
	for _, line := range wrap(strings.TrimSpace(step.Prompt), 60) {
		fmt.Printf("    %s\n", dim("› "+line))
	}
	fmt.Println()
}

// printSwitchStepReply — collapsed per-step result block. Same panel
// shape as the parallel runner uses, but slightly tighter since we'll
// often have 3–5 of these stacked in one test.
func printSwitchStepReply(idx int, step TestStep, r TestResult) {
	width := 60
	fmt.Println(dim(strings.Repeat(boxH, width)))
	header := fmt.Sprintf("step %d · %s · %ds · %s", idx+1, bold(step.CLI), r.Elapsed, r.Mode)
	fmt.Printf(" %s\n", header)
	fmt.Println(dim(strings.Repeat(boxH, width)))
	if r.Error != "" {
		fmt.Printf(" %s %s\n", red(crossMark), r.Error)
	} else {
		fmt.Println(r.Reply)
	}
	var verdict string
	switch {
	case r.Pass:
		verdict = green(checkMark + " PASS")
	case r.Transient:
		verdict = yellow(warnMark + " OVERLOAD")
		if r.FailWhy != "" {
			verdict += dim("  (" + r.FailWhy + ")")
		}
	default:
		verdict = red(crossMark + " FAIL")
		if r.FailWhy != "" {
			verdict += dim("  (" + r.FailWhy + ")")
		}
	}
	fmt.Println(dim(strings.Repeat(boxH, width)))
	fmt.Printf(" %s\n", verdict)
	fmt.Println()
}

// printSwitchStepAborted — printed when a hard FAIL ends the chain
// early. The user otherwise wonders why steps after the failure don't
// run; this names it.
func printSwitchStepAborted(after int) {
	fmt.Printf("  %s remaining steps skipped — step %d failed, later turns depend on its context\n",
		dim(dotMid), after)
	fmt.Println()
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
