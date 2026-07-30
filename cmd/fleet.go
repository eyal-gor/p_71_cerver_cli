package cmd

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	projFlag := fs.String("project", "", "Project slug to scope the board to (empty = last used; \"all\" = every project)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Project scope: --project wins and is remembered; otherwise the last
	// choice from the picker (Tab) persists across runs.
	project := loadFleetProject()
	if *projFlag != "" {
		project = *projFlag
		if project == "all" {
			project = ""
		}
		saveFleetProject(project)
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
		return fleetInteractive(ctx, gw, *limit, project)
	}

	for {
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		board, err := fleetSnapshot(reqCtx, gw, *limit, project)
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

// harnessLabel maps a cli_tool id to the product it actually runs —
// the harness is Claude Code (not the model "claude"), etc.
func harnessLabel(cli string) string {
	switch cli {
	case "claude":
		return "claude code"
	case "codex":
		return "codex"
	case "grok":
		return "grok build"
	}
	return cli
}

// questionTail: the awaiting-input heuristic — the agent's last words
// read like a question aimed at the human.
var questionTail = regexp.MustCompile(`\?\s*$`)

func fleetSnapshot(ctx context.Context, gw *gateway.Client, limit int, source string) (*fleetBoard, error) {
	var list []gateway.SessionSummary
	var err error
	if source == "" {
		list, err = gw.ListSessions(ctx, limit)
	} else {
		// Project scope: sessions are tagged with their project slug in
		// metadata.source; the list endpoint filters on it.
		var resp struct {
			Sessions []gateway.SessionSummary `json:"sessions"`
		}
		err = gw.Do(ctx, "GET", fmt.Sprintf("/v2/sessions?limit=%d&source=%s", limit, url.QueryEscape(source)), nil, &resp)
		list = resp.Sessions
	}
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
			Harness:   harnessLabel(s.CliTool()),
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
			fmt.Printf(" %s%s%s %s%-26s%s %-60s %s%-12s %s%s\n",
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
	keyTab       // project switcher
	keyRight     // project details
	keyClick     // left mouse click (x, y set)
)

// keyEvent carries the parsed key plus the rune for keyRune events and
// coordinates for clicks.
type keyEvent struct {
	k    fleetKey
	r    rune
	x, y int
}

func fleetInteractive(ctx context.Context, gw *gateway.Client, limit int, project string) error {
	saved, err := sttyGet()
	if err != nil {
		// No controlling terminal after all — fall back to plain.
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		b, err := fleetSnapshot(reqCtx, gw, limit, project)
		if err != nil {
			return err
		}
		fleetRenderPlain(b)
		return nil
	}
	if err := sttyRaw(); err != nil {
		return err
	}
	// Alt screen + hidden cursor + SGR mouse reporting: wheel events
	// reach the app as escape sequences (and scroll the view) instead of
	// iTerm scrolling its window over the alternate screen, which shows
	// ghost frames.
	fmt.Print("\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h")
	defer func() {
		fmt.Print("\x1b[?1000l\x1b[?1006l\x1b[?25h\x1b[?1049l")
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
	sessCancelled := false // Esc dismissed the wait on the pending turn
	boardLoading := true
	sessLoading := false
	collapsed := map[string]bool{} // folded board groups
	curProject := project          // "" = all projects
	var projects []gateway.Project // for the Tab switcher
	projSel := 0

	// Project details view (→ from the picker or a scoped board):
	// environment + workflow names, with browser deep-links for more.
	type detailSnap struct {
		slug string
		envs []string
		wfs  []string
	}
	detailCh := make(chan detailSnap, 1)
	detailSlug := ""
	detailLoading := false
	var detailEnvs, detailWfs []string
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
		scope := curProject
		go func() {
			reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			b, err := fleetSnapshot(reqCtx, gw, limit, scope)
			if err != nil {
				b = nil // receiver keeps the previous board
			}
			boardCh <- b
		}()
	}

	loadDetails := func(slug string) {
		detailSlug = slug
		detailLoading = true
		detailEnvs, detailWfs = nil, nil
		go func() {
			reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			snap := detailSnap{slug: slug}
			if envs, err := gw.ListEnvironments(reqCtx, slug); err == nil {
				for _, e := range envs {
					n := e.Name
					if n == "" {
						n = e.Slug
					}
					snap.envs = append(snap.envs, n)
				}
			}
			var resp struct {
				Workflows []struct {
					Name string `json:"name"`
				} `json:"workflows"`
			}
			if err := gw.Do(reqCtx, "GET", "/v2/projects/"+url.PathEscape(slug)+"/workflows", nil, &resp); err == nil {
				for _, w := range resp.Workflows {
					snap.wfs = append(snap.wfs, w.Name)
				}
			}
			detailCh <- snap
		}()
	}

	// Projects for the Tab switcher — loaded once in the background.
	go func() {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if ps, err := gw.ListProjects(reqCtx); err == nil {
			projects = ps
		}
	}()

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
			// Fixed slots — harness · model · compute always present (with
			// — when unknown) so each position reads unambiguously.
			pick := func(v string) string {
				if v == "" {
					return "—"
				}
				return v
			}
			harness, _ := s.Metadata["cli_tool"].(string)
			harness = harnessLabel(harness)
			model, _ := s.Metadata["cli_model"].(string)
			compute := ""
			if s.ComputeID != "" {
				compute = computeNames[s.ComputeID]
				if compute == "" {
					compute = shortID(s.ComputeID)
				}
			}
			snap.title = strings.Join([]string{
				name, pick(harness), pick(model), pick(compute), pick(s.Status), shortID(id),
			}, " · ")
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
			items := boardItems(board, collapsed)
			if selected >= len(items) {
				selected = len(items) - 1
			}
			if selected < 0 {
				selected = 0
			}
			selectedID = ""
			if len(items) > 0 && !items[selected].header {
				selectedID = items[selected].row.SessionID
			}
			projLabel := curProject
			for _, p := range projects {
				if p.Slug == curProject && p.Name != "" {
					projLabel = p.Name
				}
			}
			if projLabel == "" {
				projLabel = "all projects"
			}
			drawBoard(board, items, selected, &boardTop, input, launchMsg, frame, boardLoading, projLabel)
		case "projects":
			drawProjects(projects, projSel, curProject)
		case "details":
			drawDetails(detailSlug, detailEnvs, detailWfs, detailLoading, frame)
		case "session":
			drawSession(sessTitle, sessLines, &scroll, sessInput, launchMsg, frame, sessActive() && !sessCancelled, sessLoading)
		}

		select {
		case ev := <-keys:
			switch view {
			case "board":
				items := boardItems(board, collapsed)
				switch ev.k {
				case keyUp:
					if selected > 0 {
						selected--
					}
				case keyDown:
					if selected < len(items)-1 {
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
					// → toggle the fold on a header, or dive into a row.
					if task := strings.TrimSpace(input); task != "" {
						input = ""
						launchMsg = "launching…"
						launchTask(task)
					} else if len(items) > 0 && items[selected].header {
						collapsed[items[selected].group] = !collapsed[items[selected].group]
					} else if len(items) > 0 {
						r := items[selected].row
						sessName = r.Name
						sessTitle = fmt.Sprintf("%s · %s · %s · %s", r.Name, r.Harness, r.Status, shortID(r.SessionID))
						sessLines = nil
						sessStatus, sessLastRole = "", ""
						view = "session"
						scroll = 0
						sessInput = ""
						launchMsg = ""
						sessCancelled = false
						sessLoading = true
						loadSession(r.SessionID, r.Name)
					}
				case keyTab:
					view = "projects"
					projSel = 0
					for i, p := range projects {
						if p.Slug == curProject {
							projSel = i + 1 // slot 0 is "all projects"
						}
					}
				case keyRight:
					if curProject != "" {
						view = "details"
						loadDetails(curProject)
					}
				case keyClick:
					if ev.y == fleetHit.projectRow {
						view = "projects"
						projSel = 0
						for i, p := range projects {
							if p.Slug == curProject {
								projSel = i + 1
							}
						}
					} else if idx, ok := fleetHit.itemRows[ev.y]; ok && idx < len(items) {
						if items[idx].header {
							collapsed[items[idx].group] = !collapsed[items[idx].group]
						} else if idx == selected {
							// second click on the selected row dives in
							r := items[idx].row
							sessName = r.Name
							sessTitle = fmt.Sprintf("%s · %s · %s · %s", r.Name, r.Harness, r.Status, shortID(r.SessionID))
							sessLines = nil
							sessStatus, sessLastRole = "", ""
							view = "session"
							scroll = 0
							sessInput = ""
							launchMsg = ""
							sessCancelled = false
							sessLoading = true
							loadSession(r.SessionID, r.Name)
						} else {
							selected = idx
						}
					}
				case keyBack: // Esc clears the launch bar
					input = ""
				case keyQuit:
					return nil
				}
			case "projects":
				switch ev.k {
				case keyUp:
					if projSel > 0 {
						projSel--
					}
				case keyDown:
					if projSel < len(projects) {
						projSel++
					}
				case keyEnter:
					next := ""
					if projSel > 0 && projSel <= len(projects) {
						next = projects[projSel-1].Slug
					}
					curProject = next
					saveFleetProject(next)
					view = "board"
					selected, boardTop = 0, 0
					board = nil
					boardLoading = true
					loadBoard()
				case keyRight:
					if projSel > 0 && projSel <= len(projects) {
						view = "details"
						loadDetails(projects[projSel-1].Slug)
					}
				case keyClick:
					if fleetHit.pickerTop > 0 {
						idx := ev.y - fleetHit.pickerTop
						if idx >= 0 && idx <= len(projects) {
							next := ""
							if idx > 0 {
								next = projects[idx-1].Slug
							}
							curProject = next
							saveFleetProject(next)
							view = "board"
							selected, boardTop = 0, 0
							board = nil
							boardLoading = true
							loadBoard()
						}
					}
				case keyBack, keyTab:
					view = "board"
				case keyQuit:
					return nil
				}
			case "details":
				switch ev.k {
				case keyRune:
					switch ev.r {
					case 'e':
						openURL("https://cerver.ai/dashboard/environments")
						launchMsg = "opened environments in the browser"
					case 'w':
						openURL("https://cerver.ai/dashboard/workflows")
						launchMsg = "opened workflows in the browser"
					}
				case keyBack:
					view = "board"
				case keyTab:
					view = "projects"
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
						sessCancelled = false
						launchMsg = "sending…"
						sendReply(selectedID, text)
					}
				case keyBack:
					switch {
					case sessInput != "":
						sessInput = ""
					case sessActive() && !sessCancelled:
						// Cancel the wait on the pending message. There is
						// no relay-side interrupt (yet), so the agent may
						// still finish in the background — but the UI
						// stops waiting on it.
						sessCancelled = true
						launchMsg = "✕ stopped waiting — the agent may still answer in the background"
					default:
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
				if prev != "" {
					for i, it := range boardItems(board, collapsed) {
						if !it.header && it.row.SessionID == prev {
							selected = i
							break
						}
					}
				}
			}
		case d := <-detailCh:
			if d.slug == detailSlug {
				detailLoading = false
				detailEnvs = d.envs
				detailWfs = d.wfs
			}
		case s := <-sessCh:
			sessBusy = false
			sessLoading = false
			if view == "session" && s.id == selectedID {
				sessLines = s.lines
				sessStatus = s.status
				sessLastRole = s.lastRole
				sessLastAt = s.lastAt
				// The agent has answered — cancelled-wait state and any
				// lingering "✳ sent" note are stale the moment the reply
				// is on screen.
				if s.lastRole == "assistant" {
					sessCancelled = false
					if !inProgress(launchMsg) {
						launchMsg = ""
					}
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

// boardItem is one selectable line on the board: a group header (Enter
// folds/unfolds the group) or a session row (Enter opens it).
type boardItem struct {
	header bool
	folded bool
	group  string
	count  int
	row    fleetRow
}

// boardItems lists selectable items in display order, honoring folds.
func boardItems(b *fleetBoard, collapsed map[string]bool) []boardItem {
	if b == nil {
		return nil
	}
	var out []boardItem
	add := func(group string, rows []fleetRow) {
		if len(rows) == 0 {
			return
		}
		out = append(out, boardItem{header: true, folded: collapsed[group], group: group, count: len(rows)})
		if !collapsed[group] {
			for _, r := range rows {
				out = append(out, boardItem{group: group, row: r})
			}
		}
	}
	add("awaiting", b.Awaiting)
	add("working", b.Working)
	add("failed", b.Failed)
	add("completed", b.Completed)
	return out
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// fleetMascot: a little server buddy atop the board, Claude-Code
// style. Eyes blink every few seconds (driven by the spinner frame).
func fleetMascot(frame int) []string {
	eyes := "●   ●"
	if frame%25 < 2 {
		eyes = "─   ─"
	}
	return []string{
		"   ▄▀▀▀▀▀▄",
		"  ▐ " + eyes + " ▌",
		"  ▐   ◡   ▌",
		"   ▀▄▄▄▄▄▀",
		"   ▔▔▔▔▔▔▔",
	}
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

func drawBoard(b *fleetBoard, items []boardItem, selected int, top *int, input, launchMsg string, frame int, loading bool, projLabel string) {
	lines, cols := termSize()
	var sb strings.Builder
	// Home + per-line erase (\x1b[K) + erase-below (\x1b[J) instead of a
	// full clear: no blank flash between the 8fps spinner frames.
	sb.WriteString("\x1b[H")
	dim, yellow, green, red, bold, inv, reset := "\x1b[2m", "\x1b[33m", "\x1b[32m", "\x1b[31m", "\x1b[1m", "\x1b[7m", "\x1b[0m"
	eol := "\x1b[K\r\n"

	nA, nW, nDone := 0, 0, 0
	if b != nil {
		nA, nW, nDone = len(b.Awaiting), len(b.Working), len(b.Completed)+len(b.Failed)
	}
	loadTag := ""
	if loading {
		loadTag = "  " + dim + spinnerFor(frame) + reset
	}
	counts := fmt.Sprintf("%d awaiting input · %d working · %d completed%s", nA, nW, nDone, loadTag)

	fleetHit.projectRow = 0
	fleetHit.itemRows = map[int]int{}
	fleetHit.pickerTop = 0
	contentRow := 0
	logoRows := 0
	if cols >= 56 && lines >= 20 {
		// Mascot left, title + counts to its right — like the Claude
		// Code welcome block. The project line is a click target.
		m := fleetMascot(frame)
		sb.WriteString(green + m[0] + reset + eol)
		sb.WriteString(fmt.Sprintf("%s%s%s   %scerver fleet%s%s", green, m[1], reset, bold, reset, eol))
		sb.WriteString(fmt.Sprintf("%s%s%s   %s%s%s%s", green, m[2], reset, dim, counts, reset, eol))
		sb.WriteString(fmt.Sprintf("%s%s%s   %sproject:%s %s%s ▾%s %s(tab or click to switch)%s%s", green, m[3], reset, dim, reset, bold, projLabel, reset, dim, reset, eol))
		sb.WriteString(green + m[4] + reset + eol)
		sb.WriteString(eol)
		logoRows = len(m) + 1
		fleetHit.projectRow = 4
		contentRow = 7
	} else {
		sb.WriteString(fmt.Sprintf("%scerver fleet%s · %s · %sproject:%s %s ▾%s", bold, reset, counts, dim, reset, projLabel, eol))
		fleetHit.projectRow = 1
		contentRow = 2
	}

	// Column widths from the live terminal.
	nameW := 24
	metaW := 12 + 8 // harness + age
	headW := cols - nameW - metaW - 6
	if headW < 20 {
		headW = 20
	}

	// Pass 1: build every display line, noting which carries the
	// selection, so pass 2 can window the list around it — the board
	// auto-scrolls with ↑/↓. Headers are selectable (Enter folds).
	groupTitle := map[string]string{"awaiting": "Awaiting input", "working": "Working", "failed": "Failed", "completed": "Completed"}
	groupDot := map[string]string{"awaiting": yellow + "✳", "working": green + spinnerFor(frame), "failed": red + "○", "completed": dim + "·"}
	var display []string
	var lineItem []int // display line → item index (-1 = not clickable)
	push := func(ln string, item int) {
		display = append(display, ln)
		lineItem = append(lineItem, item)
	}
	selLine := 0
	for i, it := range items {
		if it.header {
			marker := "▾"
			label := groupTitle[it.group]
			if it.folded {
				marker = "▸"
				label = fmt.Sprintf("%s · %d", label, it.count)
			}
			line := dim + marker + " " + label + reset
			if i == selected {
				line = inv + marker + " " + label + reset
				selLine = len(display) + 1
			}
			push("", -1)
			push(line, i)
			continue
		}
		r := it.row
		head := r.Headline
		if head == "" {
			head = "—"
		}
		line := fmt.Sprintf("  %s%s %s%-*s%s %-*s %s%-12s %-7s%s",
			groupDot[r.group], reset,
			bold, nameW, truncate(r.Name, nameW), reset,
			headW, truncate(head, headW),
			dim, r.Harness, r.Age, reset)
		if i == selected {
			// Inverse video for the highlight; a plain dot so the whole
			// row inverts uniformly.
			dot := map[string]string{"awaiting": "✳", "working": spinnerFor(frame), "failed": "○", "completed": "·"}[r.group]
			plainLine := fmt.Sprintf("  %s %-*s %-*s %-12s %-7s",
				dot, nameW, truncate(r.Name, nameW), headW, truncate(head, headW), r.Harness, r.Age)
			line = inv + plainLine + reset
			selLine = len(display)
		}
		push(line, i)
	}
	if len(items) == 0 {
		empty := " no sessions — describe a task below to start one"
		if loading {
			empty = " " + spinnerFor(frame) + " loading fleet…"
		}
		push("", -1)
		push(dim+empty+reset, -1)
	}

	// Pass 2: window `budget` lines, nudging the stored offset only as far
	// as needed to keep the selection visible.
	budget := lines - 7 - logoRows // header block + indicators + status + launch bar + footer + slack
	if budget < 3 {
		budget = 3
	}
	if *top > selLine-2 {
		*top = selLine - 2 // keep the line above the selection visible
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
		contentRow++
	}
	for i, ln := range display[*top:end] {
		sb.WriteString(ln + eol)
		if idx := lineItem[*top+i]; idx >= 0 {
			fleetHit.itemRows[contentRow] = idx
		}
		contentRow++
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
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ select · enter open/fold · type + enter launch · tab project · ctrl-c quit%s\x1b[K", lines, dim, reset))
	os.Stdout.WriteString(sb.String())
}

// drawProjects: the Tab switcher — pick which project scopes the board.
func drawProjects(projects []gateway.Project, sel int, cur string) {
	lines, cols := termSize()
	dim, green, bold, inv, reset := "\x1b[2m", "\x1b[32m", "\x1b[1m", "\x1b[7m", "\x1b[0m"
	eol := "\x1b[K\r\n"
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	sb.WriteString(fmt.Sprintf("%sswitch project%s%s%s", bold, reset, eol, eol))

	mark := func(slug string) string {
		if slug == cur {
			return green + " ●" + reset
		}
		return "  "
	}
	line := func(i int, label, slug, meta string) {
		txt := fmt.Sprintf("%s %-32s %s%s%s", mark(slug), truncate(label, 32), dim, meta, reset)
		if i == sel {
			txt = inv + fmt.Sprintf("%s %-32s %s", map[bool]string{true: " ●", false: "  "}[slug == cur], truncate(label, 32), meta) + reset
		}
		sb.WriteString(txt + eol)
	}
	fleetHit.pickerTop = 3 // title + blank, then the first row
	fleetHit.projectRow = 0
	line(0, "all projects", "", "every session on the account")
	for i, p := range projects {
		meta := p.Slug
		if p.SessionCountMTD > 0 {
			meta = fmt.Sprintf("%s · %d sessions this month", p.Slug, p.SessionCountMTD)
		}
		label := p.Name
		if label == "" {
			label = p.Slug
		}
		line(i+1, label, p.Slug, meta)
	}
	if len(projects) == 0 {
		sb.WriteString(dim + "  loading projects…" + reset + eol)
	}
	sb.WriteString("\x1b[J")
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ select · enter switch · → details · esc cancel · ctrl-c quit%s\x1b[K", lines, dim, reset))
	_ = cols
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

func drawSession(title string, content []string, scroll *int, input, msg string, frame int, active, loading bool) {
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
	// Clamped to the content so you can't scroll past the top — the
	// counter used to run free, freezing the view while "N lines below"
	// grew on a transcript with nothing left to show.
	maxScroll := len(content) - viewport
	if maxScroll < 0 {
		maxScroll = 0
	}
	if *scroll > maxScroll {
		*scroll = maxScroll
	}
	if *scroll < 0 {
		*scroll = 0
	}
	end := len(content) - *scroll
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
	if *scroll > 0 {
		pos = fmt.Sprintf(" · %d lines below", *scroll)
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s↑↓ scroll · type + enter reply · ←/esc back · ctrl-c quit%s%s\x1b[K", lines, dim, reset, pos))
	os.Stdout.WriteString(sb.String())
}

// drawDetails: names-only view of a project's environments and
// workflows; the dashboard has the full story, one keypress away.
func drawDetails(slug string, envs, wfs []string, loading bool, frame int) {
	lines, _ := termSize()
	dim, green, bold, reset := "\x1b[2m", "\x1b[32m", "\x1b[1m", "\x1b[0m"
	eol := "\x1b[K\r\n"
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	sb.WriteString(fmt.Sprintf("%s%s%s · project details%s%s", bold, slug, reset, eol, eol))

	section := func(title string, names []string) {
		sb.WriteString(dim + title + reset + eol)
		if loading {
			sb.WriteString("  " + dim + spinnerFor(frame) + " loading…" + reset + eol)
		} else if len(names) == 0 {
			sb.WriteString("  " + dim + "none" + reset + eol)
		} else {
			max := 12
			for i, n := range names {
				if i >= max {
					sb.WriteString(fmt.Sprintf("  %s… %d more%s%s", dim, len(names)-max, reset, eol))
					break
				}
				sb.WriteString("  " + green + "·" + reset + " " + n + eol)
			}
		}
		sb.WriteString(eol)
	}
	section("environments", envs)
	section("workflows", wfs)
	sb.WriteString(dim + "more details live in the dashboard — one keypress takes you there" + reset + eol)
	sb.WriteString("\x1b[J")
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%se open environments · w open workflows · tab projects · esc back · ctrl-c quit%s\x1b[K", lines, dim, reset))
	os.Stdout.WriteString(sb.String())
}

// openURL opens a link in the default browser (macOS `open`, else xdg-open).
func openURL(u string) {
	cmd := "open"
	if _, err := exec.LookPath("xdg-open"); err == nil {
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, u).Start()
}

// ── raw-mode plumbing (stdlib-only, via stty) ────────────────────────

// fleetHit maps screen rows to click targets. Rebuilt by every draw
// (draw and event handling share the UI goroutine, so no locking).
var fleetHit struct {
	projectRow int         // header row that shows the active project
	itemRows   map[int]int // screen row → board item index
	pickerTop  int         // first selectable row of the project picker
}

// loadFleetProject / saveFleetProject: the picker's last choice lives in
// ~/.cerver/fleet_project so the board opens scoped the same way next time.
func fleetProjectPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cerver", "fleet_project")
}

func loadFleetProject() string {
	p := fleetProjectPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func saveFleetProject(slug string) {
	if p := fleetProjectPath(); p != "" {
		_ = os.WriteFile(p, []byte(slug+"\n"), 0o600)
	}
}

// mouseSeq: SGR mouse report — \x1b[<code;x;yM (press/wheel) or m (release).
var mouseSeq = regexp.MustCompile(`\x1b\[<(\d+);(\d+);(\d+)([Mm])`)

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
		case b[0] == '\t':
			out <- keyEvent{k: keyTab}
		case b[0] == '\r' || b[0] == '\n':
			out <- keyEvent{k: keyEnter}
		case b[0] == 127 || b[0] == 8: // Backspace / Ctrl-H
			out <- keyEvent{k: keyBackspace}
		case b[0] == 27: // ESC sequences — a fast autorepeat can pack
			// several arrows into one read, so emit one event apiece.
			s := string(b)
			// SGR mouse events first: wheel up/down scroll (two lines per
			// notch feels natural); clicks are ignored. Strip them so the
			// arrow-key counting below never sees their digits.
			for _, m := range mouseSeq.FindAllStringSubmatch(s, -1) {
				switch {
				case m[1] == "64":
					out <- keyEvent{k: keyUp}
					out <- keyEvent{k: keyUp}
				case m[1] == "65":
					out <- keyEvent{k: keyDown}
					out <- keyEvent{k: keyDown}
				case m[1] == "0" && m[4] == "M": // left press
					x, _ := strconv.Atoi(m[2])
					y, _ := strconv.Atoi(m[3])
					out <- keyEvent{k: keyClick, x: x, y: y}
				}
			}
			s = mouseSeq.ReplaceAllString(s, "")
			ups := strings.Count(s, "A")
			downs := strings.Count(s, "B")
			backs := strings.Count(s, "D")
			if strings.Contains(s, "C") {
				out <- keyEvent{k: keyRight}
			}
			for i := 0; i < ups; i++ {
				out <- keyEvent{k: keyUp}
			}
			for i := 0; i < downs; i++ {
				out <- keyEvent{k: keyDown}
			}
			if backs > 0 {
				out <- keyEvent{k: keyBack}
			}
			if n == 1 { // bare Esc
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
