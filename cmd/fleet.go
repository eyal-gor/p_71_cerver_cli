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
	lastAts := make([]string, len(list))
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
			last := s.Transcript[len(s.Transcript)-1]
			lastAts[i] = last.At
			if txt := s.LastAssistantText(); txt != "" && last.Role != "user" {
				tails[i] = lastLine(txt)
				roles[i] = "assistant"
				return
			}
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
		case roles[i] == "user" && recentWithin(lastAts[i], 10*time.Minute):
			// The human spoke last, minutes ago — the agent owes a reply,
			// which is what "working" means here. Status alone can't tell:
			// these relays report "ready"/"resting" even mid-run. The
			// recency window keeps long-dead unanswered sessions out.
			row.group = "working"
			board.Working = append(board.Working, row)
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
	keyBack      // ← or shift+← or Esc
	keyQuit      // Ctrl-C (and q inside the session view)
	keyRune      // printable character → the launch bar
	keyBackspace // delete in the launch bar
)

// keyEvent carries the parsed key plus the rune for keyRune events.
type keyEvent struct {
	k fleetKey
	r rune
}

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

	keys := make(chan keyEvent, 8)
	go fleetReadKeys(keys)
	launched := make(chan string, 4) // launch/reply outcome messages

	// Fetches run in goroutines and deliver here, so the spinner keeps
	// animating while we wait for the gateway.
	boardCh := make(chan *fleetBoard, 1)
	type sessSnap struct {
		id, title, status, lastRole, lastAt string
		lines                               []string
	}
	sessCh := make(chan sessSnap, 1)

	boardTick := time.NewTicker(5 * time.Second)
	defer boardTick.Stop()
	spin := time.NewTicker(120 * time.Millisecond)
	defer spin.Stop()
	frame := 0

	// UI state. view: "board" | "session".
	view := "board"
	selected := 0
	boardTop := 0
	selectedID := ""
	scroll := 0 // session view: lines scrolled UP from the bottom
	input := "" // launch bar contents
	launchMsg := ""
	sessInput := "" // session view reply bar
	sessName := ""
	sessTitle := ""
	sessStatus := ""
	sessLastRole := ""
	sessLastAt := ""
	boardLoading := true
	sessLoading := false
	boardBusy := false
	sessBusy := false
	var board *fleetBoard
	var sessLines []string

	// Compute labels for the session header (id → human name). Loaded
	// once, synchronously, before the loop starts — the map is then
	// read-only, so goroutines can use it without locking.
	computeNames := map[string]string{}
	{
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if cs, err := gw.ListComputes(reqCtx); err == nil {
			for _, c := range cs {
				computeNames[c.ID] = c.Label
			}
		}
		cancel()
	}

	loadBoard := func() {
		if boardBusy {
			return
		}
		boardBusy = true
		go func() {
			reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			b, err := fleetSnapshot(reqCtx, gw, limit)
			if err != nil {
				b = nil // receiver keeps the previous board
			}
			boardCh <- b
		}()
	}

	loadSession := func(id, name string) {
		if sessBusy {
			return
		}
		sessBusy = true
		go func() {
			reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			s, err := gw.GetSessionTail(reqCtx, id, 200)
			if err != nil {
				sessCh <- sessSnap{id: id, lines: []string{"failed to load session: " + err.Error()}}
				return
			}
			_, cols := termSize()
			snap := sessSnap{id: id, lines: renderTranscript(s, cols), status: s.Status}
			if len(s.Transcript) > 0 {
				snap.lastRole = s.Transcript[len(s.Transcript)-1].Role
				snap.lastAt = s.Transcript[len(s.Transcript)-1].At
			}
			parts := []string{name}
			if v, ok := s.Metadata["cli_tool"].(string); ok && v != "" {
				parts = append(parts, v)
			}
			if v, ok := s.Metadata["cli_model"].(string); ok && v != "" {
				parts = append(parts, v)
			}
			if s.ComputeID != "" {
				label := computeNames[s.ComputeID]
				if label == "" {
					label = shortID(s.ComputeID)
				}
				parts = append(parts, label)
			}
			if s.Status != "" {
				parts = append(parts, s.Status)
			}
			parts = append(parts, shortID(id))
			snap.title = strings.Join(parts, " · ")
			sessCh <- snap
		}()
	}

	// launchTask starts a new agent in the background — same defaults as
	// `cerver run`: claude, first ready local relay, project-scoped key.
	launchTask := func(task string) {
		go func() {
			tok, err := infisical.LoadRunToken(ctx)
			if err != nil || tok == "" {
				launched <- "launch failed: no credentials"
				return
			}
			runGw := gateway.New(tok)
			reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			computeID, err := pickCompute(reqCtx, runGw, "")
			if err != nil {
				launched <- "launch failed: " + err.Error()
				return
			}
			sid, err := runGw.CreateSession(reqCtx, gateway.SessionCreate{
				SessionType: "coding",
				Compute:     map[string]any{"compute_id": computeID},
				Task:        task,
				Workload:    "coding",
				SessionName: shortPromptLabel(task, 48),
				Metadata:    map[string]any{"cli_tool": "claude", "surface": "fleet"},
			})
			if err != nil {
				launched <- "launch failed: " + err.Error()
				return
			}
			// The create only registers the session — /input starts the agent.
			if err := runGw.SendInput(reqCtx, sid, task); err != nil {
				launched <- "launch failed: " + err.Error()
				return
			}
			launched <- "✳ launched " + shortID(sid) + " — " + shortPromptLabel(task, 40)
		}()
	}

	// sendReply pushes a follow-up into the open session — the agent
	// continues in place, same as `cerver chat`.
	sendReply := func(id, text string) {
		go func() {
			tok, err := infisical.LoadRunToken(ctx)
			if err != nil || tok == "" {
				launched <- "reply failed: no credentials"
				return
			}
			reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := gateway.New(tok).SendInput(reqCtx, id, text); err != nil {
				launched <- "reply failed: " + err.Error()
				return
			}
			launched <- "✳ sent — the agent is on it"
		}()
	}

	// sessActive: the agent on the open session is (probably) producing
	// output right now — running, or we spoke last and it hasn't replied.
	sessActive := func() bool {
		switch sessStatus {
		case "running", "provisioning", "starting":
			return true
		case "failed", "terminated":
			return false
		}
		return sessLastRole == "user" && recentWithin(sessLastAt, 10*time.Minute)
	}

	loadBoard()
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
			drawBoard(board, rows, selected, &boardTop, input, launchMsg, frame, boardLoading)
		case "session":
			drawSession(sessTitle, sessLines, scroll, sessInput, launchMsg, frame, sessActive(), sessLoading)
		}

		select {
		case ev := <-keys:
			switch view {
			case "board":
				rows := flattenBoard(board)
				switch ev.k {
				case keyUp:
					if selected > 0 {
						selected--
					}
				case keyDown:
					if selected < len(rows)-1 {
						selected++
					}
				case keyRune:
					input += string(ev.r)
				case keyBackspace:
					if r := []rune(input); len(r) > 0 {
						input = string(r[:len(r)-1])
					}
				case keyEnter:
					// Text in the launch bar → start a new agent; empty bar
					// → dive into the selected session.
					if task := strings.TrimSpace(input); task != "" {
						input = ""
						launchMsg = "launching…"
						launchTask(task)
					} else if len(rows) > 0 {
						r := rows[selected]
						sessName = r.Name
						sessTitle = fmt.Sprintf("%s · %s · %s · %s", r.Name, r.Harness, r.Status, shortID(r.SessionID))
						sessLines = nil
						sessStatus, sessLastRole = "", ""
						view = "session"
						scroll = 0
						sessInput = ""
						launchMsg = ""
						sessLoading = true
						loadSession(r.SessionID, r.Name)
					}
				case keyBack: // Esc clears the launch bar
					input = ""
				case keyQuit:
					return nil
				}
			case "session":
				switch ev.k {
				case keyUp:
					scroll++
				case keyDown:
					if scroll > 0 {
						scroll--
					}
				case keyRune:
					sessInput += string(ev.r)
				case keyBackspace:
					if r := []rune(sessInput); len(r) > 0 {
						sessInput = string(r[:len(r)-1])
					}
				case keyEnter:
					if text := strings.TrimSpace(sessInput); text != "" && selectedID != "" {
						sessInput = ""
						launchMsg = "sending…"
						sendReply(selectedID, text)
					}
				case keyBack:
					if sessInput != "" {
						sessInput = ""
					} else {
						view = "board"
						launchMsg = ""
					}
				case keyQuit:
					return nil
				}
			}
		case b := <-boardCh:
			boardBusy = false
			boardLoading = false
			if b != nil {
				prev := selectedID
				board = b
				for i, r := range flattenBoard(board) {
					if r.SessionID == prev {
						selected = i
						break
					}
				}
			}
		case s := <-sessCh:
			sessBusy = false
			sessLoading = false
			if view == "session" && s.id == selectedID {
				sessLines = s.lines
				sessStatus = s.status
				sessLastRole = s.lastRole
				sessLastAt = s.lastAt
				// The agent has answered — a lingering "✳ sent" note is
				// stale the moment the reply is on screen.
				if s.lastRole == "assistant" && !inProgress(launchMsg) {
					launchMsg = ""
				}
				if s.title != "" {
					sessTitle = s.title
				}
			}
		case msg := <-launched:
			launchMsg = msg
			if view == "session" && selectedID != "" {
				loadSession(selectedID, sessName)
			} else {
				loadBoard()
			}
		case <-boardTick.C:
			if view == "board" {
				loadBoard()
			} else if selectedID != "" {
				loadSession(selectedID, sessName)
			}
		case <-spin.C:
			frame++
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

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// fleetLogo: the big banner atop the board, Claude-Code style. Only
// rendered when the terminal has room for it.
var fleetLogo = []string{
	" ██████ ███████ ██████  ██    ██ ███████ ██████ ",
	"██      ██      ██   ██ ██    ██ ██      ██   ██",
	"██      █████   ██████  ██    ██ █████   ██████ ",
	"██      ██      ██   ██  ██  ██  ██      ██   ██",
	" ██████ ███████ ██   ██   ████   ███████ ██   ██",
}

func spinnerFor(frame int) string { return spinFrames[frame%len(spinFrames)] }

// inProgress: only the known in-flight status messages animate — a
// suffix check would false-positive on truncated text ("long task na…").
func inProgress(msg string) bool { return msg == "launching…" || msg == "sending…" }

// inputBar renders the one-line text entry as a highlighted strip: a
// dark background across the full width so the eye lands on it, green
// prompt glyph, block cursor while typing, dim placeholder otherwise.
func inputBar(input, placeholder string, cols int) string {
	bg, green, dim, bold, reset := "\x1b[48;5;236m", "\x1b[32m", "\x1b[2m", "\x1b[1m", "\x1b[0m"
	var body string
	var visible int
	if input == "" {
		body = green + "❯ " + reset + bg + dim + placeholder
		visible = 2 + len([]rune(placeholder))
	} else {
		shown := truncate(input, cols-5)
		body = green + "❯ " + reset + bg + bold + shown + reset + bg + dim + "█"
		visible = 2 + len([]rune(shown)) + 1
	}
	pad := cols - 1 - visible
	if pad < 0 {
		pad = 0
	}
	return bg + body + strings.Repeat(" ", pad) + reset
}

// recentWithin: was this RFC3339 timestamp within d of now? Used to
// decide "the agent still owes a reply" vs "that session is dead" —
// harness runs report an idle status (even "resting") while the CLI is
// actually mid-turn, so recency of the last transcript entry is the
// only trustworthy activity signal.
func recentWithin(iso string, d time.Duration) bool {
	t, err := time.Parse(time.RFC3339, iso)
	return err == nil && time.Since(t) < d
}

func drawBoard(b *fleetBoard, rows []fleetRow, selected int, top *int, input, launchMsg string, frame int, loading bool) {
	lines, cols := termSize()
	var sb strings.Builder
	// Home + per-line erase (\x1b[K) + erase-below (\x1b[J) instead of a
	// full clear: no blank flash between the 8fps spinner frames.
	sb.WriteString("\x1b[H")
	dim, yellow, green, red, bold, inv, reset := "\x1b[2m", "\x1b[33m", "\x1b[32m", "\x1b[31m", "\x1b[1m", "\x1b[7m", "\x1b[0m"
	eol := "\x1b[K\r\n"

	logoRows := 0
	if cols >= 52 && lines >= 22 {
		for _, ln := range fleetLogo {
			sb.WriteString(green + ln + reset + eol)
		}
		sb.WriteString(eol) // breathing room under the banner
		logoRows = len(fleetLogo) + 1
	}

	nA, nW, nDone := 0, 0, 0
	if b != nil {
		nA, nW, nDone = len(b.Awaiting), len(b.Working), len(b.Completed)+len(b.Failed)
	}
	loadTag := ""
	if loading {
		loadTag = "  " + dim + spinnerFor(frame) + reset
	}
	sb.WriteString(fmt.Sprintf("%scerver fleet%s · %d awaiting input · %d working · %d completed%s%s", bold, reset, nA, nW, nDone, loadTag, eol))

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
	groupDot := map[string]string{"awaiting": yellow + "✳", "working": green + spinnerFor(frame), "failed": red + "○", "completed": dim + "·"}
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
			// Inverse video for the highlight; a plain dot so the whole
			// row inverts uniformly.
			dot := map[string]string{"awaiting": "✳", "working": spinnerFor(frame), "failed": "○", "completed": "·"}[r.group]
			plainLine := fmt.Sprintf(" %s %-*s %-*s %-7s %-7s",
				dot, nameW, truncate(r.Name, nameW), headW, truncate(head, headW), r.Harness, r.Age)
			line = inv + plainLine + reset
			selLine = len(display)
		}
		display = append(display, line)
	}
	if len(rows) == 0 {
		empty := " no sessions — describe a task below to start one"
		if loading {
			empty = " " + spinnerFor(frame) + " loading fleet…"
		}
		display = append(display, "", dim+empty+reset)
	}

	// Pass 2: window `budget` lines, nudging the stored offset only as far
	// as needed to keep the selection visible (with one line of margin so
	// the next row is always peeking).
	budget := lines - 8 - logoRows // logo + header + indicators + status + launch bar + footer + slack
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
		sb.WriteString(fmt.Sprintf("%s ↑ %d above%s%s", dim, *top, reset, eol))
	}
	for _, ln := range display[*top:end] {
		sb.WriteString(ln + eol)
	}
	if end < len(display) {
		sb.WriteString(fmt.Sprintf("%s ↓ %d below%s%s", dim, len(display)-end, reset, eol))
	}
	sb.WriteString("\x1b[J") // clear everything below before the bottom chrome

	// Bottom chrome: launch status, the launch bar, key hints.
	if launchMsg != "" {
		msg := launchMsg
		if inProgress(msg) {
			msg = spinnerFor(frame) + " " + msg
		}
		sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s%s%s\x1b[K", lines-2, dim, truncate(msg, cols-1), reset))
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s\x1b[K", lines-1, inputBar(input, "describe a task for a new agent…", cols)))
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ select · enter open · type + enter launch · esc clear · ctrl-c quit%s\x1b[K", lines, dim, reset))
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
		// Relay bookkeeping: after every turn a session_completed system
		// event lands in the transcript (exit code, duration, usage).
		// It's plumbing, not conversation — hide it. Other system events
		// (errors etc.) stay visible.
		if e.Role == "system" && strings.HasPrefix(strings.TrimSpace(e.Content), "{") &&
			strings.Contains(e.Content, `"session_completed"`) {
			continue
		}
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

func drawSession(title string, content []string, scroll int, input, msg string, frame int, active, loading bool) {
	lines, cols := termSize()
	dim, bold, reset := "\x1b[2m", "\x1b[1m", "\x1b[0m"
	eol := "\x1b[K\r\n"
	var sb strings.Builder
	sb.WriteString("\x1b[H")

	viewport := lines - 6 // bottom chrome: info line + status + reply bar + footer + slack
	if viewport < 3 {
		viewport = 3
	}
	// While the agent owes a reply, render a pending bubble exactly where
	// the answer will appear — the real message replaces it on arrival.
	if active || msg == "sending…" {
		content = append(append([]string{}, content...),
			"", dim+"── agent ──"+reset, dim+"  "+spinnerFor(frame)+" thinking…"+reset)
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
		sb.WriteString(ln + eol)
	}
	sb.WriteString("\x1b[J")

	// Status line: thinking/sending render inline as the pending bubble
	// above; this line only carries loads and non-transient messages.
	status := ""
	switch {
	case msg == "sending…": // inline bubble covers it
	case inProgress(msg):
		status = spinnerFor(frame) + " " + msg
	case loading:
		status = spinnerFor(frame) + " loading transcript…"
	case msg != "":
		status = msg
	}
	// Bottom stack: status → highlighted reply bar → session identity →
	// key hints.
	if status != "" {
		sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s%s%s\x1b[K", lines-3, dim, truncate(status, cols-1), reset))
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s\x1b[K", lines-2, inputBar(input, "reply to this agent…", cols)))
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s%s%s\x1b[K", lines-1, bold, truncate(title, cols-1), reset))
	pos := ""
	if scroll > 0 {
		pos = fmt.Sprintf(" · %d lines below", scroll)
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ scroll · type + enter reply · ←/esc back · ctrl-c quit%s%s\x1b[K", lines, dim, reset, pos))
	os.Stdout.WriteString(sb.String())
}

// ── raw-mode plumbing (stdlib-only, via stty) ────────────────────────

func fleetReadKeys(out chan<- keyEvent) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		b := buf[:n]
		switch {
		case b[0] == 3: // Ctrl-C
			out <- keyEvent{k: keyQuit}
		case b[0] == '\r' || b[0] == '\n':
			out <- keyEvent{k: keyEnter}
		case b[0] == 127 || b[0] == 8: // Backspace / Ctrl-H
			out <- keyEvent{k: keyBackspace}
		case b[0] == 27: // ESC sequences
			s := string(b)
			switch {
			case strings.HasSuffix(s, "A"):
				out <- keyEvent{k: keyUp}
			case strings.HasSuffix(s, "B"):
				out <- keyEvent{k: keyDown}
			case strings.HasSuffix(s, "D"): // ← and shift+← ("[1;2D") both go back
				out <- keyEvent{k: keyBack}
			case n == 1: // bare Esc
				out <- keyEvent{k: keyBack}
			}
		default:
			// Printable text feeds the launch bar (board) or acts as a
			// shortcut (session view decides per rune).
			for _, r := range string(b) {
				if r >= 32 && r != 127 {
					out <- keyEvent{k: keyRune, r: r}
				}
			}
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
