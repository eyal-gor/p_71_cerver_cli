package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Fleet renders the agent-manager board: every recent session bucketed
// into awaiting-input / working / completed, with a one-line headline
// pulled from each session's transcript tail. The terminal sibling of
// the dashboard's Fleet view.
//
// On a TTY it's interactive: ↑/↓ move the selection, Enter dives into
// the session transcript (thinking, tool calls, replies), ←/shift+←
// goes back, q quits. Piped (or with --plain) it prints once and exits.
//
//	cerver fleet                 interactive board (plain when piped)
//	cerver fleet --plain         one-shot static board
//	cerver fleet --watch         plain board, redrawn every 5s
//	cerver fleet --limit 50
//	cerver fleet --json          raw grouped JSON for scripting
func Fleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	limit := fs.Int("limit", 30, "Max sessions to fetch")
	watch := fs.Bool("watch", false, "Plain mode: redraw every 5s")
	plain := fs.Bool("plain", false, "Print the board once, no interactivity")
	jsonOut := fs.Bool("json", false, "Emit grouped JSON instead of the board")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return fmt.Errorf("no cerver credentials — run cerver.ai/install.sh or `cerver login`")
	}
	gw := gateway.New(tok)

	interactive := !*plain && !*watch && !*jsonOut && stdoutIsTTY() && stdinIsTTY()
	if interactive {
		return fleetInteractive(ctx, gw, *limit)
	}

	for {
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		board, err := fleetSnapshot(reqCtx, gw, *limit)
		cancel()
		if err != nil {
			return err
		}
		if *jsonOut {
			return jsonEncode(os.Stdout, board)
		}
		if *watch {
			fmt.Print("\x1b[2J\x1b[H") // clear + home
		}
		fleetRenderPlain(board)
		if !*watch {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

// ── data ─────────────────────────────────────────────────────────────

// fleetRow is one line on the board.
type fleetRow struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Headline  string `json:"headline"`
	Harness   string `json:"harness"`
	Status    string `json:"status"`
	Age       string `json:"age"`
	group     string
}

type fleetBoard struct {
	Awaiting  []fleetRow `json:"awaiting_input"`
	Working   []fleetRow `json:"working"`
	Failed    []fleetRow `json:"failed"`
	Completed []fleetRow `json:"completed"`
}

// questionTail: the awaiting-input heuristic — the agent's last words
// read like a question aimed at the human.
var questionTail = regexp.MustCompile(`\?\s*$`)

func fleetSnapshot(ctx context.Context, gw *gateway.Client, limit int) (*fleetBoard, error) {
	list, err := gw.ListSessions(ctx, limit)
	if err != nil {
		return nil, err
	}

	// Transcript tails for the freshest rows only — each is a full
	// session GET, so cap the fan-out and run it concurrently.
	const tailRows = 15
	tails := make([]string, len(list))
	roles := make([]string, len(list))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range list {
		if i >= tailRows {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := gw.GetSessionTail(ctx, list[i].SessionID, 4)
			if err != nil || len(s.Transcript) == 0 {
				return
			}
			// Prefer what the agent said over trailing system events
			// (session_completed JSON etc.).
			if txt := s.LastAssistantText(); txt != "" {
				tails[i] = lastLine(txt)
				roles[i] = "assistant"
				return
			}
			last := s.Transcript[len(s.Transcript)-1]
			tails[i] = lastLine(last.Content)
			roles[i] = last.Role
		}(i)
	}
	wg.Wait()

	board := &fleetBoard{}
	for i, s := range list {
		headline := tails[i]
		if headline == "" {
			if v, ok := s.Metadata["task"].(string); ok {
				headline = lastLine(v)
			}
		}
		row := fleetRow{
			SessionID: s.SessionID,
			Name:      s.SessionName,
			Headline:  headline,
			Harness:   s.CliTool(),
			Status:    s.Status,
			Age:       humanTime(s.UpdatedAt),
		}
		if row.Name == "" {
			row.Name = shortID(s.SessionID)
		}
		switch {
		case s.Status == "running" || s.Status == "provisioning" || s.Status == "starting":
			row.group = "working"
			board.Working = append(board.Working, row)
		case s.Status == "failed" || s.Status == "terminated":
			row.group = "failed"
			board.Failed = append(board.Failed, row)
		case roles[i] == "assistant" && questionTail.MatchString(strings.TrimSpace(tails[i])):
			row.group = "awaiting"
			board.Awaiting = append(board.Awaiting, row)
		default:
			row.group = "completed"
			board.Completed = append(board.Completed, row)
		}
	}
	return board, nil
}

// lastLine returns the last content-bearing line of a message — the
// natural one-line headline for "what did this agent just say". Pure
// markdown scaffolding (fences, table rules, separators) is skipped,
// and emphasis/heading markers are stripped so the board reads clean.
var mdScaffold = regexp.MustCompile("^[`|#>*\\-=_+\\s]+$")

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || mdScaffold.MatchString(l) {
			continue
		}
		l = strings.ReplaceAll(l, "**", "")
		l = strings.Trim(l, "*_`#> ")
		if l != "" {
			return l
		}
	}
	return ""
}

// ── plain (non-interactive) rendering ────────────────────────────────

func fleetRenderPlain(b *fleetBoard) {
	color := fleetColorEnabled()
	dim, yellow, green, red, bold, reset := "", "", "", "", "", ""
	if color {
		dim, yellow, green, red, bold, reset = "\x1b[2m", "\x1b[33m", "\x1b[32m", "\x1b[31m", "\x1b[1m", "\x1b[0m"
	}

	fmt.Printf("%scerver fleet%s · %d awaiting input · %d working · %d completed\n",
		bold, reset, len(b.Awaiting), len(b.Working), len(b.Completed)+len(b.Failed))

	group := func(title, dot, dotColor string, rows []fleetRow, max int) {
		if len(rows) == 0 {
			return
		}
		fmt.Printf("\n%s%s%s\n", dim, title, reset)
		if max > 0 && len(rows) > max {
			rows = rows[:max]
		}
		for _, r := range rows {
			head := r.Headline
			if head == "" {
				head = "—"
			}
			fmt.Printf(" %s%s%s %s%-26s%s %-60s %s%-7s %s%s\n",
				dotColor, dot, reset,
				bold, truncate(r.Name, 26), reset,
				truncate(head, 60),
				dim, r.Harness, r.Age, reset)
		}
	}
	group("Awaiting input", "✳", yellow, b.Awaiting, 0)
	group("Working", "●", green, b.Working, 0)
	group("Failed", "○", red, b.Failed, 5)
	group("Completed", "·", dim, b.Completed, 15)

	fmt.Printf("\n%scerver run \"task\" to start · cerver show <id> for the transcript · cerver chat <id> to reply%s\n", dim, reset)
}

// ── interactive TUI ──────────────────────────────────────────────────
//
// stdlib-only raw mode: `stty raw -echo` on the way in, the saved
// `stty -g` state on the way out. Alternate screen buffer so the
// user's scrollback survives the session.

type fleetKey int

const (
	keyNone fleetKey = iota
	keyUp
	keyDown
	keyEnter
	keyBack // ← or shift+← or Esc
	keyQuit // q or Ctrl-C
)

func fleetInteractive(ctx context.Context, gw *gateway.Client, limit int) error {
	saved, err := sttyGet()
	if err != nil {
		// No controlling terminal after all — fall back to plain.
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		b, err := fleetSnapshot(reqCtx, gw, limit)
		if err != nil {
			return err
		}
		fleetRenderPlain(b)
		return nil
	}
	if err := sttyRaw(); err != nil {
		return err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l") // alt screen, hide cursor
	defer func() {
		fmt.Print("\x1b[?25h\x1b[?1049l") // show cursor, main screen
		sttyRestore(saved)
	}()

	keys := make(chan fleetKey, 8)
	go fleetReadKeys(keys)

	boardTick := time.NewTicker(5 * time.Second)
	defer boardTick.Stop()

	// UI state. view: "board" | "session".
	view := "board"
	selected := 0
	boardTop := 0
	selectedID := ""
	scroll := 0 // session view: lines scrolled UP from the bottom
	var board *fleetBoard
	var sessLines []string
	var sessTitle string

	fetchBoard := func() {
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if b, err := fleetSnapshot(reqCtx, gw, limit); err == nil {
			board = b
		}
	}
	fetchSession := func(id string) {
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		s, err := gw.GetSessionTail(reqCtx, id, 200)
		if err != nil {
			sessLines = []string{"failed to load session: " + err.Error()}
			return
		}
		_, cols := termSize()
		sessLines = renderTranscript(s, cols)
	}

	fetchBoard()
	for {
		switch view {
		case "board":
			rows := flattenBoard(board)
			if selected >= len(rows) {
				selected = len(rows) - 1
			}
			if selected < 0 {
				selected = 0
			}
			if len(rows) > 0 {
				selectedID = rows[selected].SessionID
			}
			drawBoard(board, rows, selected, &boardTop)
		case "session":
			drawSession(sessTitle, sessLines, scroll)
		}

		select {
		case k := <-keys:
			switch view {
			case "board":
				rows := flattenBoard(board)
				switch k {
				case keyUp:
					if selected > 0 {
						selected--
					}
				case keyDown:
					if selected < len(rows)-1 {
						selected++
					}
				case keyEnter:
					if len(rows) > 0 {
						r := rows[selected]
						sessTitle = fmt.Sprintf("%s · %s · %s · %s", r.Name, r.Harness, r.Status, shortID(r.SessionID))
						sessLines = []string{"loading…"}
						view = "session"
						scroll = 0
						fetchSession(r.SessionID)
					}
				case keyQuit, keyBack:
					if k == keyQuit {
						return nil
					}
				}
			case "session":
				switch k {
				case keyUp:
					scroll++
				case keyDown:
					if scroll > 0 {
						scroll--
					}
				case keyBack:
					view = "board"
				case keyQuit:
					return nil
				}
			}
		case <-boardTick.C:
			if view == "board" {
				prev := selectedID
				fetchBoard()
				// Keep the highlight on the same session across refreshes.
				for i, r := range flattenBoard(board) {
					if r.SessionID == prev {
						selected = i
						break
					}
				}
			} else if selectedID != "" {
				// Live session: keep the transcript fresh; if the user is
				// at the bottom they stay pinned to the newest output.
				fetchSession(selectedID)
			}
		}
	}
}

// flattenBoard lists selectable rows in display order.
func flattenBoard(b *fleetBoard) []fleetRow {
	if b == nil {
		return nil
	}
	out := make([]fleetRow, 0, len(b.Awaiting)+len(b.Working)+len(b.Failed)+len(b.Completed))
	out = append(out, b.Awaiting...)
	out = append(out, b.Working...)
	out = append(out, b.Failed...)
	out = append(out, b.Completed...)
	return out
}

func drawBoard(b *fleetBoard, rows []fleetRow, selected int, top *int) {
	lines, cols := termSize()
	var sb strings.Builder
	sb.WriteString("\x1b[H\x1b[2J")
	dim, yellow, green, red, bold, inv, reset := "\x1b[2m", "\x1b[33m", "\x1b[32m", "\x1b[31m", "\x1b[1m", "\x1b[7m", "\x1b[0m"

	nA, nW, nDone := 0, 0, 0
	if b != nil {
		nA, nW, nDone = len(b.Awaiting), len(b.Working), len(b.Completed)+len(b.Failed)
	}
	sb.WriteString(fmt.Sprintf("%scerver fleet%s · %d awaiting input · %d working · %d completed\r\n", bold, reset, nA, nW, nDone))

	// Column widths from the live terminal.
	nameW := 24
	metaW := 8 + 8 // harness + age
	headW := cols - nameW - metaW - 6
	if headW < 20 {
		headW = 20
	}

	// Pass 1: build every display line (group headers + rows), noting
	// which line carries the selected row, so pass 2 can window the list
	// around the selection — the board auto-scrolls with ↑/↓.
	groupTitle := map[string]string{"awaiting": "Awaiting input", "working": "Working", "failed": "Failed", "completed": "Completed"}
	groupDot := map[string]string{"awaiting": yellow + "✳", "working": green + "●", "failed": red + "○", "completed": dim + "·"}
	var display []string
	selLine := 0
	prevGroup := ""
	for i, r := range rows {
		if r.group != prevGroup {
			display = append(display, "", dim+groupTitle[r.group]+reset)
			prevGroup = r.group
		}
		head := r.Headline
		if head == "" {
			head = "—"
		}
		line := fmt.Sprintf(" %s%s %s%-*s%s %-*s %s%-7s %-7s%s",
			groupDot[r.group], reset,
			bold, nameW, truncate(r.Name, nameW), reset,
			headW, truncate(head, headW),
			dim, r.Harness, r.Age, reset)
		if i == selected {
			// Inverse video for the highlight; strip inner color codes so
			// the whole row inverts uniformly.
			plainLine := fmt.Sprintf(" %s %-*s %-*s %-7s %-7s",
				strings.TrimLeft(groupDot[r.group], "\x1b[0123456789;m"),
				nameW, truncate(r.Name, nameW), headW, truncate(head, headW), r.Harness, r.Age)
			line = inv + plainLine + reset
			selLine = len(display)
		}
		display = append(display, line)
	}
	if len(rows) == 0 {
		display = append(display, "", dim+" no sessions — cerver run \"task\" starts one"+reset)
	}

	// Pass 2: window `budget` lines, nudging the stored offset only as far
	// as needed to keep the selection visible (with one line of margin so
	// the next row is always peeking).
	budget := lines - 5 // header + footer + overflow indicators + slack
	if budget < 3 {
		budget = 3
	}
	if *top > selLine-2 {
		*top = selLine - 2 // keep the group header above the selection visible
	}
	if *top < selLine-budget+2 {
		*top = selLine - budget + 2
	}
	if *top > len(display)-budget {
		*top = len(display) - budget
	}
	if *top < 0 {
		*top = 0
	}
	end := *top + budget
	if end > len(display) {
		end = len(display)
	}
	if *top > 0 {
		sb.WriteString(fmt.Sprintf("%s ↑ %d above%s\r\n", dim, *top, reset))
	}
	for _, ln := range display[*top:end] {
		sb.WriteString(ln + "\r\n")
	}
	if end < len(display) {
		sb.WriteString(fmt.Sprintf("%s ↓ %d below%s\r\n", dim, len(display)-end, reset))
	}

	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ select · enter open · q quit%s", lines, dim, reset))
	os.Stdout.WriteString(sb.String())
}

// renderTranscript flattens a session's transcript into display lines:
// role-labelled, word-wrapped, tool/thinking entries dimmed — the
// "see how it thinks" view.
func renderTranscript(s *gateway.Session, cols int) []string {
	dim, cyan, bold, reset := "\x1b[2m", "\x1b[36m", "\x1b[1m", "\x1b[0m"
	width := cols - 4
	if width < 40 {
		width = 40
	}
	var out []string
	for _, e := range s.Transcript {
		label := e.Role
		style := ""
		switch {
		case e.Role == "user":
			label, style = "you", cyan
		case e.Role == "assistant" && (e.Kind == "" || e.Kind == "text"):
			label, style = "agent", bold
		case e.Role == "assistant":
			label, style = "agent · "+e.Kind, dim
		default:
			if e.Kind != "" {
				label = e.Role + " · " + e.Kind
			}
			style = dim
		}
		out = append(out, fmt.Sprintf("%s── %s ──%s", style, label, reset))
		body := strings.TrimSpace(e.Content)
		if body == "" {
			body = "—"
		}
		bodyStyle := ""
		if style == dim {
			bodyStyle = dim
		}
		for _, ln := range strings.Split(body, "\n") {
			for _, w := range wrapLine(ln, width) {
				out = append(out, bodyStyle+"  "+w+reset)
			}
		}
		out = append(out, "")
	}
	if len(out) == 0 {
		out = []string{dim + "empty transcript" + reset}
	}
	return out
}

func wrapLine(s string, width int) []string {
	r := []rune(s)
	if len(r) <= width {
		return []string{s}
	}
	var out []string
	for len(r) > width {
		cut := width
		// Prefer breaking at a space near the edge.
		for j := width; j > width-20 && j > 0; j-- {
			if r[j] == ' ' {
				cut = j
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
		for len(r) > 0 && r[0] == ' ' {
			r = r[1:]
		}
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

func drawSession(title string, content []string, scroll int) {
	lines, _ := termSize()
	dim, bold, reset := "\x1b[2m", "\x1b[1m", "\x1b[0m"
	var sb strings.Builder
	sb.WriteString("\x1b[H\x1b[2J")
	sb.WriteString(fmt.Sprintf("%s%s%s\r\n\r\n", bold, title, reset))

	viewport := lines - 4
	if viewport < 3 {
		viewport = 3
	}
	// scroll counts lines up from the bottom; 0 = pinned to newest.
	end := len(content) - scroll
	if end > len(content) {
		end = len(content)
	}
	if end < viewport {
		end = min(viewport, len(content))
	}
	start := end - viewport
	if start < 0 {
		start = 0
	}
	for _, ln := range content[start:end] {
		sb.WriteString(ln + "\r\n")
	}

	pos := ""
	if scroll > 0 {
		pos = fmt.Sprintf(" · %d lines below", scroll)
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ scroll · ←/shift+←/esc back · q quit%s%s", lines, dim, pos, reset))
	os.Stdout.WriteString(sb.String())
}

// ── raw-mode plumbing (stdlib-only, via stty) ────────────────────────

func fleetReadKeys(out chan<- fleetKey) {
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		b := buf[:n]
		switch {
		case b[0] == 3 || b[0] == 'q': // Ctrl-C / q
			out <- keyQuit
		case b[0] == '\r' || b[0] == '\n':
			out <- keyEnter
		case b[0] == 27: // ESC sequences
			s := string(b)
			switch {
			case strings.HasSuffix(s, "A"):
				out <- keyUp
			case strings.HasSuffix(s, "B"):
				out <- keyDown
			case strings.HasSuffix(s, "D"): // ← and shift+← ("[1;2D") both go back
				out <- keyBack
			case n == 1: // bare Esc
				out <- keyBack
			}
		case b[0] == 'k':
			out <- keyUp
		case b[0] == 'j':
			out <- keyDown
		}
	}
}

func sttyGet() (string, error) {
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sttyRaw() error {
	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func sttyRestore(saved string) {
	cmd := exec.Command("stty", saved)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

// termSize returns (rows, cols), defaulting to 40×120 when stty can't
// tell (e.g. odd environments) so rendering still works.
func termSize() (int, int) {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err == nil {
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) == 2 {
			r, err1 := strconv.Atoi(parts[0])
			c, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil && r > 0 && c > 0 {
				return r, c
			}
		}
	}
	return 40, 120
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// fleetColorEnabled: honor NO_COLOR and skip ANSI when stdout isn't a
// terminal (pipes, CI).
func fleetColorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return stdoutIsTTY()
}
