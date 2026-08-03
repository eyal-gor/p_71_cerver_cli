package cmd

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
// Bare `cerver agents` is the front door; `cerver fleet` is the same command
// and is where the flags live, because `cerver agents --json` already means
// "list my saved agent definitions as JSON".
//
//	cerver agents                interactive board (plain when piped)
//	cerver fleet                 the same thing, spelled the old way
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

	fmt.Printf("%scerver agents%s · %d awaiting input · %d working · %d completed\n",
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
	keyResend    // Ctrl-R: nudge — resend the last user message
	keyPalette   // Ctrl-P: the command palette
	keyHover     // pointer moved (x, y set) — no button held
)

// paletteCmd is one row in the Ctrl-P command palette: what it does, and
// the key that does it directly for anyone who already knows.
type paletteCmd struct {
	id    string
	label string
	key   string
}

// fleetPalette: the commands reachable from a view. This is the only place
// key bindings are spelled out now — the footer just points at ctrl+p
// instead of listing five bindings on every screen.
func fleetPalette(view string) []paletteCmd {
	switch view {
	case "session":
		return []paletteCmd{
			{"reply", "Reply to this agent", "type + enter"},
			{"copyreply", "Copy the last reply", ""},
			{"copyall", "Copy the whole transcript", ""},
			{"mouse", "Pause mouse — drag to select text", ""},
			{"model", "Change model / harness", ""},
			{"resend", "Resend the last message", "ctrl+r"},
			{"back", "Back to the board", "esc"},
			{"dashboard", "Open the dashboard", ""},
			{"quit", "Quit", "ctrl+c"},
		}
	case "projects":
		return []paletteCmd{
			{"switch", "Switch to the selected project", "enter"},
			{"details", "Project details", "→"},
			{"back", "Back to the board", "esc"},
			{"quit", "Quit", "ctrl+c"},
		}
	case "details":
		return []paletteCmd{
			{"envs", "Open environments", "e"},
			{"wfs", "Open workflows", "w"},
			{"projects", "Switch project", "tab"},
			{"back", "Back to the board", "esc"},
			{"quit", "Quit", "ctrl+c"},
		}
	}
	return []paletteCmd{
		{"open", "Open the selected session", "enter"},
		{"launch", "New agent — type a task below", "type + enter"},
		{"project", "Switch project", "tab"},
		{"details", "Project details", "→"},
		{"refresh", "Refresh the board", "ctrl+r"},
		{"dashboard", "Open the dashboard", ""},
		{"quit", "Quit", "ctrl+c"},
	}
}

// launchOpt is one row in the launch picker: a harness, and the model to
// pin on it. An empty model means "whatever that harness defaults to".
type launchOpt struct {
	label string
	cli   string
	model string
}

// harnessModels: the models worth putting in front of you per harness. The
// string lands in metadata.cli_model, which the relay hands straight to that
// harness's --model flag — so these are the harness's own aliases, not pinned
// version ids, which would go stale on every model release.
//
// A harness with no entry here still gets its "default" row, so a provider
// added to the relay shows up without waiting for a CLI release.
func harnessModels(cli string) []string {
	switch cli {
	case "claude":
		return []string{"opus", "sonnet", "haiku"}
	case "codex":
		return []string{"gpt-5", "gpt-5-codex", "gpt-5-mini"}
	case "grok":
		return []string{"grok-4", "grok-4-fast"}
	case "gemma":
		// Google's open-weights family, served free (rate-limited) through
		// the Gemini API — no local weights, no key beyond GEMINI_API_KEY.
		return []string{"gemma-4-31b-it", "gemma-4-12b-it"}
	}
	// ollama deliberately has no entry: its models are files on a specific
	// machine, so the only honest source is that compute's own report.
	return nil
}

// harnessOrder is the order harnesses appear in the picker. Anything the
// relay reports that isn't listed lands after these, alphabetically.
var harnessOrder = []string{"claude", "codex", "grok", "gemma", "ollama"}

// fleetLaunchOptions builds the picker from what the relay actually reports
// it can run, rather than a constant in this file. That constant was already
// wrong: relays ship gemma, and a hardcoded claude/codex/grok list silently
// hid it.
func fleetLaunchOptions(ready []string, local map[string][]string) []launchOpt {
	seen := map[string]bool{}
	ordered := []string{}
	for _, h := range harnessOrder {
		for _, r := range ready {
			if r == h && !seen[h] {
				seen[h] = true
				ordered = append(ordered, h)
			}
		}
	}
	extra := []string{}
	for _, r := range ready {
		if !seen[r] && r != "" {
			seen[r] = true
			extra = append(extra, r)
		}
	}
	sort.Strings(extra)
	ordered = append(ordered, extra...)

	// Nothing reported (relay offline, or an older relay that doesn't send
	// capabilities) — fall back to the harnesses that have always existed
	// so the picker is never empty.
	if len(ordered) == 0 {
		ordered = harnessOrder
	}

	opts := make([]launchOpt, 0, len(ordered)*3)
	for _, h := range ordered {
		// A machine's own models win over the static table — for ollama
		// that's the only truth there is, and offering a model this
		// compute hasn't pulled would just fail at spawn.
		models := local[h]
		if len(models) == 0 {
			models = harnessModels(h)
		}
		opts = append(opts, launchOpt{h + " · default", h, ""})
		for _, m := range models {
			opts = append(opts, launchOpt{h + " · " + m, h, m})
		}
	}
	return opts
}

// localModels merges every compute's per-machine model inventory. Models
// are deduped across computes but not attributed to one: the launcher picks
// the compute, so an ollama model pulled on only one machine can still be
// chosen — the relay resolves the tag, and reports honestly if it's missing.
func localModels(computes []gateway.Compute) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	for i := range computes {
		for harness, models := range computes[i].Capabilities.LocalModels {
			for _, m := range models {
				key := harness + "|" + m
				if m == "" || seen[key] {
					continue
				}
				seen[key] = true
				out[harness] = append(out[harness], m)
			}
		}
	}
	for h := range out {
		sort.Strings(out[h])
	}
	return out
}

// fleetModelPath / loadLaunchIndex / saveLaunchChoice: the picker opens on
// whatever you launched last, so the common case is Enter-Enter.
func fleetModelPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cerver", "fleet_model")
}

func saveLaunchChoice(cli, model string) {
	if p := fleetModelPath(); p != "" {
		_ = os.WriteFile(p, []byte(cli+"|"+model+"\n"), 0o600)
	}
}

func loadLaunchIndex(opts []launchOpt) int {
	p := fleetModelPath()
	if p == "" {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	saved := strings.TrimSpace(string(b))
	for i, o := range opts {
		if o.cli+"|"+o.model == saved {
			return i
		}
	}
	return 0
}

// harnessReady: which harnesses the live relay actually reports. Picking
// one it can't run fails at spawn time with a worse error than a ✗ here.
func harnessReady(computes []gateway.Compute) map[string]bool {
	ready := map[string]bool{}
	for i := range computes {
		c := &computes[i]
		for _, t := range c.Capabilities.CliTools {
			ready[t] = true
		}
	}
	return ready
}

// harnessNames is harnessReady as an ordered slice — what the picker is
// built from.
func harnessNames(computes []gateway.Compute) []string {
	ready := harnessReady(computes)
	out := make([]string, 0, len(ready))
	for k := range ready {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// drawLaunchSheet shows the harness/model picker as a centered modal.
func drawLaunchSheet(opts []launchOpt, sel int, task string, ready map[string]bool, switching bool) {
	green, red, reset := "\x1b[32m", "\x1b[31m", "\x1b[0m"
	rows := make([]modalRow, 0, len(opts))
	for _, o := range opts {
		mark := ""
		if len(ready) > 0 {
			mark = green + "✓" + reset
			if !ready[o.cli] {
				mark = red + "✗" + reset
			}
		}
		rows = append(rows, modalRow{label: o.label, key: defaultLabel(o.model), mark: mark})
	}
	title, sub := "launch on", task
	if switching {
		// Be explicit about the cost before they press Enter: a model change
		// is free, a harness change restarts the agent.
		title = "switch this session to"
		sub = "model change keeps the agent · harness change restarts it"
	}
	fleetHit.itemRows = map[int]int{}
	fleetHit.projectRow = 0
	fleetHit.pickerTop = drawModal(title, sub, rows, sel)
}

// defaultLabel spells out what an empty model means, rather than leaving
// the column blank and looking like a rendering bug.
func defaultLabel(model string) string {
	if model == "" {
		return "its default model"
	}
	return model
}

// palMatches filters the palette by the typed query — plain
// case-insensitive substring, which is all a seven-item list needs.
func palMatches(cmds []paletteCmd, q string) []paletteCmd {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return cmds
	}
	out := make([]paletteCmd, 0, len(cmds))
	for _, c := range cmds {
		if strings.Contains(strings.ToLower(c.label), q) {
			out = append(out, c)
		}
	}
	return out
}

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
	// 1003 (any-motion) is what makes hover possible at all — 1000 only
	// reports presses. It is chatty: every pointer move sends a report, so
	// the loop redraws only when the hovered row actually changes.
	fmt.Print("\x1b[?1049h\x1b[?25l")
	setMouseReporting(true)
	defer func() {
		setMouseReporting(false)
		fmt.Print("\x1b[?25h\x1b[?1049l")
		sttyRestore(saved)
	}()

	keys := make(chan keyEvent, 8)
	go fleetReadKeys(keys)
	launched := make(chan string, 4) // launch/reply outcome messages

	// Fetches run in goroutines and deliver here, so the spinner keeps
	// animating while we wait for the gateway.
	boardCh := make(chan *fleetBoard, 1)
	type sessSnap struct {
		id, title, status, lastRole, lastAt, lastUser, lastReply string
		lines                                                    []string
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
	sessLastUser := ""
	sessLastReply := "" // the agent's latest text, for "copy the last reply"
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
	var relayComputes []gateway.Compute
	{
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if cs, err := gw.ListComputes(reqCtx); err == nil {
			relayComputes = cs
			for _, c := range cs {
				computeNames[c.ID] = c.Label
			}
		}
		cancel()
	}
	relayCh := make(chan []gateway.Compute, 1)
	relayTick := time.NewTicker(30 * time.Second)
	defer relayTick.Stop()

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
			harnessLbl, _ := s.Metadata["cli_tool"].(string)
			snap := sessSnap{id: id, lines: renderTranscript(s, cols, harnessLabel(harnessLbl)), status: s.Status}
			snap.lastReply = strings.TrimSpace(s.LastAssistantText())
			if len(s.Transcript) > 0 {
				snap.lastRole = s.Transcript[len(s.Transcript)-1].Role
				snap.lastAt = s.Transcript[len(s.Transcript)-1].At
			}
			for i := len(s.Transcript) - 1; i >= 0; i-- {
				if s.Transcript[i].Role == "user" {
					snap.lastUser = strings.TrimSpace(s.Transcript[i].Content)
					break
				}
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

	// launchTask starts a new agent in the background. cli/model come from
	// the launch picker; compute is the first ready local relay and auth
	// the project-scoped key — same defaults as `cerver run`. An empty
	// model means "the harness's own default", which is what the relay
	// does with a missing metadata.cli_model.
	launchTask := func(task, cli, model string) {
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
			md := map[string]any{"cli_tool": cli, "surface": "fleet"}
			if model != "" {
				md["cli_model"] = model
			}
			sid, err := runGw.CreateSession(reqCtx, gateway.SessionCreate{
				SessionType: "coding",
				Compute:     map[string]any{"compute_id": computeID},
				Task:        task,
				Workload:    "coding",
				SessionName: shortPromptLabel(task, 48),
				Metadata:    md,
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

	// switchModel repoints a live session at a different model (and
	// optionally harness) from its next turn onward.
	switchModel := func(id, cli, model string) {
		go func() {
			tok, err := infisical.LoadRunToken(ctx)
			if err != nil || tok == "" {
				launched <- "switch failed: no credentials"
				return
			}
			reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := gateway.New(tok).SwitchModel(reqCtx, id, cli, model); err != nil {
				launched <- "switch failed: " + err.Error()
				return
			}
			launched <- "✳ switched to " + cli + "/" + defaultLabel(model) + " — takes effect on your next message"
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

	// Command palette (ctrl+p): an overlay on top of the current view. It
	// owns the keyboard while open; picking a row synthesises the key that
	// command is bound to, so there is exactly one code path per action.
	palOpen, palSel, palQuery := false, 0, ""

	// Launch picker: Enter with text in the bar opens it, so choosing the
	// harness/model is on the path to starting an agent rather than a
	// setting hidden somewhere else. Opens on your last choice.
	// launchSwitch flips the same sheet from "start a new agent on X" to
	// "move this session to X".
	launchOpen, launchSel, launchText := false, 0, ""
	var launchList []launchOpt

	// hoverY is the screen row the pointer is on, 0 when it's elsewhere.
	// Motion reports arrive continuously, so this only ever triggers a
	// repaint when the row changes — otherwise moving the mouse across the
	// window would repaint on every pixel.
	hoverY := 0

	// mouseOn: the app is listening to the mouse. Turned off on request so
	// the terminal's own drag-to-select works — the two cannot coexist.
	mouseOn := true
	launchSwitch := false

	loadBoard()
	// needDraw gates the repaint. The spinner ticks 8×/sec, and repainting
	// a whole view that nothing on screen animates is what made the
	// overlays flicker.
	// drawView paints whatever view is current. Pulled out of the loop so
	// the modal path can repaint the background exactly once, dimmed.
	drawView := func() {
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
			if len(items) > 0 && !items[selected].header && !items[selected].project {
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
			drawBoard(board, items, selected, &boardTop, input, launchMsg, frame, boardLoading, projLabel, relayStatusLine(relayComputes))
		case "projects":
			drawProjects(projects, projSel, curProject)
		case "details":
			drawDetails(detailSlug, detailEnvs, detailWfs, detailLoading, frame)
		case "session":
			owed := sessLastRole == "user" && !sessCancelled && sessStatus != "failed" && sessStatus != "terminated"
			quiet := time.Duration(0)
			if t, err := time.Parse(time.RFC3339, sessLastAt); err == nil {
				quiet = time.Since(t)
			}
			drawSession(sessTitle, sessLines, &scroll, sessInput, launchMsg, frame, sessActive() && !sessCancelled, sessLoading, owed, quiet, hoverY)
		}
	}

	needDraw := true
	prevOverlay := false
	for {
		// While a sheet is open it owns the screen, and the view underneath
		// is frozen anyway — repainting it only to immediately cover it up
		// is exactly the flicker. Paint the sheet alone; the view is still
		// on screen from the frame before it opened.
		overlay := palOpen || launchOpen
		// Opening or closing a modal always redraws, whatever the tick said:
		// the background has to change intensity on exactly that frame.
		if overlay != prevOverlay {
			needDraw = true
		}
		// Synchronized output (DEC 2026): the terminal holds the frame back
		// until the closing marker, so a repaint lands in one piece instead
		// of being shown mid-write. Terminals that don't know the sequence
		// ignore it.
		if needDraw {
			os.Stdout.WriteString("\x1b[?2026h")
		}
		// The view behind a modal is repainted exactly once — dimmed — on the
		// frame the modal opens, and once more at full strength when it
		// closes. In between it's frozen: repainting it every spinner tick
		// only to cover it again is what made the modal flicker. Both the dim
		// background and the modal land inside one synchronized frame, so the
		// two never appear separately.
		if needDraw && overlay != prevOverlay {
			dimScreen = overlay
			drawView()
			dimScreen = false
			prevOverlay = overlay
		} else if needDraw && !overlay {
			drawView()
		}
		if needDraw && launchOpen {
			drawLaunchSheet(launchList, launchSel, launchText, harnessReady(relayComputes), launchSwitch)
		}
		if needDraw && palOpen {
			drawPalette(palMatches(fleetPalette(view), palQuery), palSel, palQuery)
		}
		if needDraw {
			os.Stdout.WriteString("\x1b[?2026l")
		}
		needDraw = true

		select {
		case ev := <-keys:
			if ev.k == keyHover {
				if ev.y == hoverY {
					needDraw = false // same row — nothing on screen changed
				} else {
					hoverY = ev.y
				}
				break
			}
			if ev.k == keyPalette && !palOpen && !launchOpen {
				palOpen, palSel, palQuery = true, 0, ""
				break
			}
			if palOpen {
				cmds := palMatches(fleetPalette(view), palQuery)
				chosen := -1
				switch ev.k {
				case keyUp:
					if palSel > 0 {
						palSel--
					}
				case keyDown:
					if palSel < len(cmds)-1 {
						palSel++
					}
				case keyRune:
					palQuery += string(ev.r)
					palSel = 0
				case keyBackspace:
					if r := []rune(palQuery); len(r) > 0 {
						palQuery = string(r[:len(r)-1])
					}
					palSel = 0
				case keyEnter:
					chosen = palSel
				case keyClick:
					chosen = ev.y - fleetHit.pickerTop
				case keyBack, keyPalette, keyTab:
					palOpen, palQuery = false, ""
				case keyQuit:
					return nil
				}
				if chosen < 0 || chosen >= len(cmds) {
					break
				}
				palOpen, palQuery = false, ""
				// Translate the pick into the key it is bound to, then fall
				// through to the view's normal handler. Two commands have no
				// binding to borrow, so they act here.
				switch cmds[chosen].id {
				case "open", "switch":
					ev = keyEvent{k: keyEnter}
				case "project", "projects":
					ev = keyEvent{k: keyTab}
				case "details":
					ev = keyEvent{k: keyRight}
				case "refresh", "resend":
					ev = keyEvent{k: keyResend}
				case "envs":
					ev = keyEvent{k: keyRune, r: 'e'}
				case "wfs":
					ev = keyEvent{k: keyRune, r: 'w'}
				case "quit":
					return nil
				case "back":
					// Straight back — not keyBack, which in the session view
					// first clears the reply bar or cancels a pending wait.
					view = "board"
					launchMsg = ""
					ev = keyEvent{k: keyNone}
				case "dashboard":
					openURL("https://cerver.ai/dashboard")
					launchMsg = "opened the dashboard in the browser"
					ev = keyEvent{k: keyNone}
				case "launch":
					// Only meaningful with a task already typed; otherwise
					// the bar is focused and waiting anyway.
					if t := strings.TrimSpace(input); t != "" {
						launchList = fleetLaunchOptions(harnessNames(relayComputes), localModels(relayComputes))
						launchText, launchSel, launchOpen, launchSwitch = t, loadLaunchIndex(launchList), true, false
					}
					ev = keyEvent{k: keyNone}
				case "copyreply":
					if err := copyToClipboard(sessLastReply); err != nil {
						launchMsg = "copy failed: " + err.Error()
					} else {
						launchMsg = "✳ copied the last reply"
					}
					ev = keyEvent{k: keyNone}
				case "copyall":
					plain := make([]string, 0, len(sessLines))
					for _, l := range sessLines {
						plain = append(plain, strings.TrimRight(stripANSI(l), " "))
					}
					if err := copyToClipboard(strings.Join(plain, "\n")); err != nil {
						launchMsg = "copy failed: " + err.Error()
					} else {
						launchMsg = fmt.Sprintf("✳ copied %d lines", len(plain))
					}
					ev = keyEvent{k: keyNone}
				case "mouse":
					// Mouse reporting eats drag-to-select: while the app is
					// listening, the terminal hands drags to us instead of
					// making a selection. Turn it off so the terminal's own
					// selection works, and say how to get it back.
					mouseOn = !mouseOn
					setMouseReporting(mouseOn)
					if mouseOn {
						launchMsg = "mouse back on"
					} else {
						launchMsg = "mouse off — drag to select, then ctrl+p to turn it back on"
					}
					ev = keyEvent{k: keyNone}
				case "model":
					if selectedID != "" {
						launchList = fleetLaunchOptions(harnessNames(relayComputes), localModels(relayComputes))
						launchText, launchSel, launchOpen, launchSwitch = "", loadLaunchIndex(launchList), true, true
					}
					ev = keyEvent{k: keyNone}
				default: // "reply" — the input bar is already focused
					ev = keyEvent{k: keyNone}
				}
				if ev.k == keyNone {
					break
				}
			}
			if launchOpen {
				switch ev.k {
				case keyUp:
					if launchSel > 0 {
						launchSel--
					}
				case keyDown:
					if launchSel < len(launchList)-1 {
						launchSel++
					}
				case keyEnter, keyClick:
					idx := launchSel
					if ev.k == keyClick {
						idx = ev.y - fleetHit.pickerTop
					}
					if idx < 0 || idx >= len(launchList) {
						break
					}
					o := launchList[idx]
					saveLaunchChoice(o.cli, o.model)
					if launchSwitch {
						launchMsg = "switching…"
						switchModel(selectedID, o.cli, o.model)
					} else {
						launchMsg = "launching…"
						launchTask(launchText, o.cli, o.model)
					}
					launchOpen, input, launchText = false, "", ""
				case keyBack:
					launchOpen = false
					if !launchSwitch {
						// Cancel back to the task still sitting in the bar, so
						// a mis-hit Enter doesn't cost you what you typed.
						input = launchText
					}
					launchText = ""
				case keyQuit:
					return nil
				}
				break
			}
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
					// Text in the launch bar → pick the harness/model, which
					// launches; empty bar → toggle the fold on a header, or
					// dive into a row.
					if task := strings.TrimSpace(input); task != "" {
						launchList = fleetLaunchOptions(harnessNames(relayComputes), localModels(relayComputes))
						launchText, launchSel, launchOpen = task, loadLaunchIndex(launchList), true
					} else if len(items) > 0 && items[selected].project {
						view = "projects"
						projSel = 0
						for i, p := range projects {
							if p.Slug == curProject {
								projSel = i + 1
							}
						}
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
					if len(items) > 0 && items[selected].project {
						view = "projects"
						projSel = 0
						for i, p := range projects {
							if p.Slug == curProject {
								projSel = i + 1
							}
						}
					} else if curProject != "" {
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
				case keyResend: // refresh the board
					boardLoading = true
					loadBoard()
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
						sessLastUser = text
						launchMsg = "sending…"
						sendReply(selectedID, text)
					}
				case keyResend:
					if sessLastUser != "" && selectedID != "" {
						sessCancelled = false
						launchMsg = "sending…"
						sendReply(selectedID, sessLastUser)
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
				sessLastUser = s.lastUser
				sessLastReply = s.lastReply
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
		case cs := <-relayCh:
			relayComputes = cs
			for _, c := range cs {
				computeNames[c.ID] = c.Label
			}
		case <-relayTick.C:
			go func() {
				reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				if cs, err := gw.ListComputes(reqCtx); err == nil {
					relayCh <- cs
				}
			}()
		case <-spin.C:
			// Keep the frame counter moving so spinners are in the right
			// phase when a sheet closes — but nothing inside a sheet
			// animates, so don't repaint on its account.
			frame++
			if palOpen || launchOpen {
				needDraw = false
			}
		}
	}
}

// boardItem is one selectable line on the board: a group header (Enter
// folds/unfolds the group) or a session row (Enter opens it).
type boardItem struct {
	project bool // the header's project line — Enter opens the switcher
	header  bool
	folded  bool
	group   string
	count   int
	row     fleetRow
}

// boardItems lists selectable items in display order, honoring folds.
func boardItems(b *fleetBoard, collapsed map[string]bool) []boardItem {
	if b == nil {
		return nil
	}
	// The project line is selectable too, so the keyboard can reach the
	// switcher without knowing about Tab.
	out := []boardItem{{project: true}}
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

// fleetMascot: a square little server buddy atop the board. One eye
// per model company (landing palette), blinking every few seconds; the
// ground strip is the landing page's colorful divider.
func fleetMascot(frame int) []string {
	const (
		cBlue   = "\x1b[38;2;42;120;214m"
		cOrange = "\x1b[38;2;235;104;52m"
		cGreen  = "\x1b[38;2;27;175;122m"
		cYellow = "\x1b[38;2;237;161;0m"
		cReset  = "\x1b[0m"
	)
	eyeL, eyeR := cBlue+"■"+cReset, cOrange+"■"+cReset
	if frame%25 < 2 {
		eyeL, eyeR = cBlue+"─"+cReset, cOrange+"─"+cReset
	}
	strip := " " + cBlue + "▀▀" + cOrange + "▀▀" + cGreen + "▀▀" + cYellow + "▀▀" + cReset
	return []string{
		" ▛▀▀▀▀▀▀▀▜",
		" ▌ " + eyeL + "   " + eyeR + " ▐",
		" ▌   " + cYellow + "◡" + cReset + "   ▐",
		" ▙▄▄▄▄▄▄▄▟",
		strip,
	}
}

func spinnerFor(frame int) string { return spinFrames[frame%len(spinFrames)] }

// inProgress: only the known in-flight status messages animate — a
// suffix check would false-positive on truncated text ("long task na…").
func inProgress(msg string) bool { return msg == "launching…" || msg == "sending…" }

// inputBar renders the one-line text entry as a highlighted strip: a
// dark background across the full width so the eye lands on it, green
// prompt glyph, block cursor while typing, dim placeholder otherwise.
// inputCard renders the prompt as a two-line card: an accent rail down the
// left, the prompt itself, then a row of actions with their keys. Borrowed
// shape from the permission cards in opencode — the point is that the thing
// you type into and the things you can do with it read as one object,
// instead of a bare line with the bindings exiled to a footer.
func inputCard(input, placeholder string, actions [][2]string, cols int, focused bool) []string {
	bg, green, dim, bold, reset := "\x1b[48;5;235m", "\x1b[32m", "\x1b[2m", "\x1b[1m", "\x1b[0m"
	rail := green + "▌" + reset
	if !focused {
		rail = dim + "▌" + reset
	}
	inner := cols - 2
	if inner < 10 {
		inner = 10
	}

	var body string
	var visible int
	if input == "" {
		body = green + "❯ " + reset + bg + dim + truncate(placeholder, inner-3)
		visible = 2 + len([]rune(truncate(placeholder, inner-3)))
	} else {
		shown := truncate(input, inner-4)
		body = green + "❯ " + reset + bg + bold + shown + reset + bg + dim + "█"
		visible = 2 + len([]rune(shown)) + 1
	}
	prompt := rail + bg + " " + body + strings.Repeat(" ", maxi(0, inner-visible-1)) + reset

	// Action row: key in normal weight, what it does dimmed after it.
	var parts []string
	width := 0
	for _, a := range actions {
		seg := a[0] + dim + " " + a[1] + reset
		if width+len([]rune(a[0]))+len([]rune(a[1]))+4 > inner-2 {
			break
		}
		width += len([]rune(a[0])) + len([]rune(a[1])) + 4
		parts = append(parts, seg)
	}
	hints := rail + bg + " " + dim + strings.Join(parts, dim+"   ") + reset + bg +
		strings.Repeat(" ", maxi(0, inner-width)) + reset
	return []string{prompt, hints}
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// sideInfo is what the context column shows about the open session.
type sideInfo struct {
	id, status, harness, model, compute string
	inTok, outTok                       int
}

// sidebarWidth: the context column only earns its space on a wide terminal;
// below that the transcript needs every column it can get.
func sidebarWidth(cols int) int {
	if cols < 104 {
		return 0
	}
	return 26
}

// sidebarLines builds the right-hand context column — the session's identity
// and cost in one place, so the transcript doesn't have to carry it.
func sidebarLines(si sideInfo, rows int) []string {
	dim, bold, green, reset := "\x1b[2m", "\x1b[1m", "\x1b[32m", "\x1b[0m"
	var out []string
	head := func(s string) { out = append(out, dim+strings.ToUpper(s)+reset) }
	val := func(s string) { out = append(out, truncate(s, 24)) }
	gap := func() { out = append(out, "") }

	head("session")
	val(bold + shortID(si.id) + reset)
	mark := dim + "●" + reset
	if si.status == "running" {
		mark = green + "●" + reset
	}
	val(mark + " " + si.status)
	gap()

	head("harness")
	val(pickOr(si.harness, "—"))
	if si.model != "" {
		out = append(out, dim+truncate(si.model, 24)+reset)
	}
	gap()

	head("compute")
	val(pickOr(si.compute, "—"))
	gap()

	if si.inTok > 0 || si.outTok > 0 {
		head("tokens")
		val(fmt.Sprintf("%s in", humanCount(float64(si.inTok))))
		val(fmt.Sprintf("%s out", humanCount(float64(si.outTok))))
	}

	for len(out) < rows {
		out = append(out, "")
	}
	return out[:rows]
}

func pickOr(v, alt string) string {
	if strings.TrimSpace(v) == "" {
		return alt
	}
	return v
}

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

func drawBoard(b *fleetBoard, items []boardItem, selected int, top *int, input, launchMsg string, frame int, loading bool, projLabel, relayLine string) {
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
	projSelected := len(items) > 0 && items[0].project && selected == 0
	contentRow := 0
	logoRows := 0
	if cols >= 56 && lines >= 20 {
		// Mascot left, title + counts to its right — like the Claude
		// Code welcome block. The project line is a click target.
		m := fleetMascot(frame)
		sb.WriteString(m[0] + eol)
		sb.WriteString(fmt.Sprintf("%s   %scerver agents%s%s", m[1], bold, reset, eol))
		sb.WriteString(fmt.Sprintf("%s   %s%s%s%s", m[2], dim, counts, reset, eol))
		projLine := fmt.Sprintf("%sproject:%s %s%s ▾%s %s(tab or click to switch)%s", dim, reset, bold, projLabel, reset, dim, reset)
		if projSelected {
			projLine = inv + "project: " + projLabel + " ▾" + reset
		}
		sb.WriteString(fmt.Sprintf("%s   %s%s", m[3], projLine, eol))
		sb.WriteString(fmt.Sprintf("%s   %s%s", m[4], relayLine, eol))
		sb.WriteString(eol)
		logoRows = len(m) + 1
		fleetHit.projectRow = 4
		contentRow = 7
	} else {
		small := fmt.Sprintf("%sproject:%s %s ▾", dim, reset, projLabel)
		if projSelected {
			small = inv + "project: " + projLabel + " ▾" + reset
		}
		sb.WriteString(fmt.Sprintf("%scerver agents%s · %s · %s%s", bold, reset, counts, small, eol))
		sb.WriteString(relayLine + eol)
		fleetHit.projectRow = 1
		contentRow = 3
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
		if it.project {
			continue // rendered in the header block, not the list
		}
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
	if projSelected {
		selLine = 0
	}
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
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s\x1b[K", lines, fleetFooter(projLabel, cols)))
	paint(sb.String())
}

// dimScreen makes the next paint recede: everything is forced to low
// intensity, and bold/inverse are flattened. Set while a modal is open so
// the view behind it reads as background rather than competing with it —
// the terminal equivalent of dropping the opacity.
var dimScreen bool

// paint writes a rendered frame, applying the dim treatment when armed.
// Every draw function goes through here so there is one place that decides
// how a frame reaches the screen.
func paint(s string) {
	if dimScreen {
		// Re-enter dim after every reset, and flatten the two attributes
		// that would otherwise punch through it.
		r := strings.NewReplacer(
			"\x1b[0m", "\x1b[0m\x1b[2m",
			"\x1b[1m", "\x1b[2m",
			"\x1b[7m", "\x1b[2m",
		)
		s = "\x1b[2m" + r.Replace(s)
	}
	os.Stdout.WriteString(s)
}

// modalRow is one line in a centered modal: a label, and the key that does
// the same thing directly.
type modalRow struct {
	label string
	key   string
	// mark is an optional leading glyph (already coloured) — used by the
	// launch picker for harness availability.
	mark string
}

// drawModal renders a centered, bordered box over the current view. Shared
// by the command palette and the launch picker so both read as the same
// object rather than two different bottom-anchored strips.
//
// Returns the screen row of the first selectable line, for click mapping.
func drawModal(title, subtitle string, rows []modalRow, sel int) int {
	lines, cols := termSize()
	dim, bold, inv, reset := "\x1b[2m", "\x1b[1m", "\x1b[7m", "\x1b[0m"

	// Width fits the widest row, within sane bounds.
	inner := len([]rune(title)) + 4
	for _, r := range rows {
		if w := len([]rune(r.label)) + len([]rune(r.key)) + 8; w > inner {
			inner = w
		}
	}
	if w := len([]rune(subtitle)) + 4; w > inner {
		inner = w
	}
	if max := cols - 6; inner > max {
		inner = max
	}
	if inner < 24 {
		inner = 24
	}

	body := len(rows)
	if body == 0 {
		body = 1
	}
	height := body + 4 // top border, title, subtitle, bottom border
	top := (lines - height) / 2
	if top < 1 {
		top = 1
	}
	left := (cols-inner)/2 + 1
	if left < 1 {
		left = 1
	}

	var sb strings.Builder
	at := func(row int, s string) {
		sb.WriteString(fmt.Sprintf("\x1b[%d;%dH%s", row, left, s))
	}
	bar := strings.Repeat("─", inner-2)
	at(top, dim+"╭"+bar+"╮"+reset)
	at(top+1, dim+"│ "+reset+bold+truncate(title, inner-4)+reset+
		strings.Repeat(" ", maxInt(0, inner-4-len([]rune(truncate(title, inner-4)))))+dim+" │"+reset)
	sub := truncate(subtitle, inner-4)
	at(top+2, dim+"│ "+reset+dim+sub+strings.Repeat(" ", maxInt(0, inner-4-len([]rune(sub))))+" │"+reset)

	rowTop := top + 3
	if len(rows) == 0 {
		at(rowTop, dim+"│ "+reset+dim+fmt.Sprintf("%-*s", inner-4, "no match")+dim+" │"+reset)
	}
	for i, r := range rows {
		labelW := inner - 6 - len([]rune(r.key))
		if labelW < 4 {
			labelW = 4
		}
		mark := r.mark
		if mark == "" {
			mark = " "
		}
		line := fmt.Sprintf("%s %-*s %s", mark, labelW, truncate(r.label, labelW), dim+r.key+reset)
		if i == sel {
			line = inv + fmt.Sprintf("%s %-*s %s", mark, labelW, truncate(r.label, labelW), r.key) + reset
		}
		// Pad to the border regardless of the escape codes inside.
		visible := 1 + 1 + labelW + 1 + len([]rune(r.key))
		at(rowTop+i, dim+"│ "+reset+line+strings.Repeat(" ", maxInt(0, inner-4-visible))+dim+" │"+reset)
	}
	at(top+3+body, dim+"╰"+bar+"╯"+reset)
	paint(sb.String())
	return rowTop
}

// fleetFooter is the one status line every view ends with: where you are
// on the left, a single pointer at the command palette on the right. The
// bindings themselves live in ctrl+p, not on screen.
func fleetFooter(left string, cols int) string {
	dim, reset := "\x1b[2m", "\x1b[0m"
	right := "ctrl+p commands"
	if mousePaused {
		right = "mouse off · ctrl+p commands"
	}
	// The key stays at full strength while everything around it dims: it is
	// the one thing on this row you might need to act on, and a footer where
	// the hint is as faint as the context is a footer nobody reads.
	bright := strings.Replace(right, "ctrl+p", reset+"ctrl+p"+dim, 1)
	if cols < len(right)+6 {
		return dim + bright + reset
	}
	left = truncate(left, cols-len(right)-4)
	pad := cols - len([]rune(left)) - len(right) - 2
	if pad < 1 {
		pad = 1
	}
	return dim + " " + left + strings.Repeat(" ", pad) + bright + reset
}

// drawPalette shows the command list as a centered modal over the dimmed
// view behind it.
func drawPalette(cmds []paletteCmd, sel int, query string) {
	rows := make([]modalRow, 0, len(cmds))
	for _, c := range cmds {
		rows = append(rows, modalRow{label: c.label, key: c.key})
	}
	sub := "↑↓ select · enter run · esc close"
	if query != "" {
		sub = "› " + query
	}
	fleetHit.itemRows = map[int]int{}
	fleetHit.projectRow = 0
	fleetHit.pickerTop = drawModal("commands", sub, rows, sel)
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
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s\x1b[K", lines, fleetFooter("switch project", cols)))
	paint(sb.String())
}

// renderTranscript flattens a session's transcript into display lines:
// role-labelled, word-wrapped, tool/thinking entries dimmed — the
// "see how it thinks" view.
// renderTranscript turns a session into display lines.
//
// Layout is a speaker header — icon, name, right-aligned time — followed by
// the body indented two spaces. Deliberately NOT a gutter rail: a bar down
// the left edge looks good and ruins copy/paste, because every selected line
// drags the bar along with it. Indentation reads the same and copies clean.
func renderTranscript(s *gateway.Session, cols int, harness string) []string {
	dim, cyan, green, bold, reset := "\x1b[2m", "\x1b[36m", "\x1b[32m", "\x1b[1m", "\x1b[0m"
	width := cols - 4
	if width < 40 {
		width = 40
	}
	agentName := harness
	if agentName == "" {
		agentName = "agent"
	}
	var out []string
	prevUser := ""
	lastLabel := ""
	for _, e := range s.Transcript {
		// cerver run registers the task at create AND sends it as the
		// first input — the same text lands twice back-to-back. Show it once.
		if e.Role == "user" {
			if strings.TrimSpace(e.Content) == prevUser {
				continue
			}
			prevUser = strings.TrimSpace(e.Content)
		} else if e.Role == "assistant" {
			prevUser = ""
		}
		// Relay bookkeeping: after every turn a session_completed system
		// event lands in the transcript (exit code, duration, usage). It's
		// plumbing, not conversation — hide it. Other system events stay.
		if e.Role == "system" && strings.HasPrefix(strings.TrimSpace(e.Content), "{") &&
			strings.Contains(e.Content, `"session_completed"`) {
			continue
		}

		icon, label, style := "·", e.Role, dim
		bodyStyle := ""
		switch {
		case e.Role == "user":
			icon, label, style = "▍", "you", cyan+bold
		case e.Role == "assistant" && (e.Kind == "" || e.Kind == "text"):
			icon, label, style = "◆", agentName, green+bold
		case e.Role == "assistant":
			// Thinking and tool calls are the agent working, not talking —
			// one dim glyph so they read as texture, not as turns.
			icon, label, bodyStyle = "⋯", e.Kind, dim
		default:
			if e.Kind != "" {
				label = e.Role + " · " + e.Kind
			}
			bodyStyle = dim
		}

		// Consecutive entries from the same speaker (thinking, then a tool
		// call, then the answer) don't each need a header.
		if label != lastLabel {
			out = append(out, speakerLine(icon, label, shortClock(e.At), style, dim, width))
			lastLabel = label
		}

		body := strings.TrimSpace(e.Content)
		if body == "" {
			body = "—"
		}
		for _, ln := range strings.Split(body, "\n") {
			for _, w := range wrapLine(ln, width-2) {
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

// speakerLine renders "◆ agent                         12:04".
func speakerLine(icon, label, clock, style, dim string, width int) string {
	reset := "\x1b[0m"
	left := icon + " " + label
	pad := width - len([]rune(left)) - len([]rune(clock))
	if pad < 1 {
		pad = 1
	}
	return style + left + reset + strings.Repeat(" ", pad) + dim + clock + reset
}

// shortClock renders an ISO timestamp as HH:MM, or "" when unparseable —
// a wrong time is worse than none.
func shortClock(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04")
}

// stripANSI removes styling so copied text is text.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func stripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// mousePaused mirrors the state for the footer, so "why isn't clicking
// working" is answered on screen rather than remembered.
var mousePaused bool

// setMouseReporting turns the app's mouse listening on or off. Off hands
// drag-to-select back to the terminal, which is the only way to select text
// with the mouse while a full-screen app is running.
func setMouseReporting(on bool) {
	mousePaused = !on
	if on {
		fmt.Print("\x1b[?1000h\x1b[?1003h\x1b[?1006h")
		return
	}
	fmt.Print("\x1b[?1003l\x1b[?1000l\x1b[?1006l")
}

// copyToClipboard puts text on the system clipboard. Uses the local
// pasteboard when there is one, else OSC 52, which also works over ssh
// (if the terminal allows clipboard access).
func copyToClipboard(text string) error {
	for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	os.Stdout.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a")
	return nil
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

func drawSession(title string, content []string, scroll *int, input, msg string, frame int, active, loading, owed bool, quiet time.Duration, hoverY int) {
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
	// Long silences say so and offer the nudge; very stale ones (the
	// relay never woke an agent) drop the spinner but keep the hint.
	if active || owed || msg == "sending…" {
		think := spinnerFor(frame) + " thinking…"
		switch {
		case msg == "sending…":
			// fresh send — plain thinking bubble
		case active && quiet > 90*time.Second:
			think = spinnerFor(frame) + " still quiet · " + shortDur(quiet) + " — ctrl-r resends your message"
		case !active && owed:
			think = "no reply · quiet " + shortDur(quiet) + " — ctrl-r resends your message"
		}
		content = append(append([]string{}, content...),
			"", dim+"── agent ──"+reset, dim+"  "+think+reset)
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
	// The identity line is reference material, not something you read every
	// turn — dim so it stops competing with the transcript, and bring it
	// back to full strength when the pointer is on it.
	idStyle := dim
	if hoverY == lines-1 {
		idStyle = bold
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s%s%s\x1b[K", lines-1, idStyle, truncate(title, cols-1), reset))
	pos := "transcript"
	if *scroll > 0 {
		pos = fmt.Sprintf("%d lines below", *scroll)
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s\x1b[K", lines, fleetFooter(pos, cols)))
	paint(sb.String())
}

// drawDetails: names-only view of a project's environments and
// workflows; the dashboard has the full story, one keypress away.
func drawDetails(slug string, envs, wfs []string, loading bool, frame int) {
	lines, cols := termSize()
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
	sb.WriteString(fmt.Sprintf("\x1b[%d;1H%s\x1b[K", lines, fleetFooter(slug+" · project details", cols)))
	paint(sb.String())
}

// relayStatusLine: liveness of the local relay and which harnesses it
// can run — judged by heartbeat freshness, because compute Status can
// say "ready" long after the relay process died.
func relayStatusLine(computes []gateway.Compute) string {
	dim, green, red, reset := "\x1b[2m", "\x1b[32m", "\x1b[31m", "\x1b[0m"
	var best *gateway.Compute
	var bestAge time.Duration
	for i := range computes {
		c := &computes[i]
		if c.Kind != "local" {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.LastHeartbeatAt)
		if err != nil {
			continue
		}
		age := time.Since(t)
		if best == nil || age < bestAge {
			best, bestAge = c, age
		}
	}
	if best == nil {
		return dim + "relay " + red + "○" + reset + dim + " not installed — curl -fsSL cerver.ai/install.sh | bash" + reset
	}
	if bestAge > 3*time.Minute {
		return fmt.Sprintf("%srelay %s○%s%s %s · offline %s — agents can't run until it's back%s",
			dim, red, reset, dim, best.Label, shortDur(bestAge), reset)
	}
	line := fmt.Sprintf("%srelay %s●%s %s", dim, green, reset+dim, best.Label)
	avail := map[string]bool{}
	for _, t := range best.Capabilities.CliTools {
		avail[t] = true
	}
	for _, h := range []string{"claude", "codex", "grok"} {
		if avail[h] {
			line += " · " + reset + green + "✓" + reset + dim + " " + harnessLabel(h)
		} else {
			line += " · ✗ " + harnessLabel(h)
		}
	}
	return line + reset
}

// shortDur: compact duration for status text — 3m, 2h, 1d.
func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
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
		case b[0] == 0x12: // Ctrl-R
			out <- keyEvent{k: keyResend}
		case b[0] == 0x10: // Ctrl-P
			out <- keyEvent{k: keyPalette}
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
				case m[1] == "35": // motion, no button held → hover
					x, _ := strconv.Atoi(m[2])
					y, _ := strconv.Atoi(m[3])
					out <- keyEvent{k: keyHover, x: x, y: y}
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
