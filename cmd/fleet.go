package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
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
//	cerver fleet                 one-shot board
//	cerver fleet --watch         redraw every 5s until Ctrl-C
//	cerver fleet --limit 50
//	cerver fleet --json          raw grouped JSON for scripting
func Fleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	limit := fs.Int("limit", 30, "Max sessions to fetch")
	watch := fs.Bool("watch", false, "Redraw every 5s")
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
		fleetRender(board)
		if !*watch {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

// fleetRow is one line on the board.
type fleetRow struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Headline  string `json:"headline"`
	Harness   string `json:"harness"`
	Status    string `json:"status"`
	Age       string `json:"age"`
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
			board.Working = append(board.Working, row)
		case s.Status == "failed" || s.Status == "terminated":
			board.Failed = append(board.Failed, row)
		case roles[i] == "assistant" && questionTail.MatchString(strings.TrimSpace(tails[i])):
			board.Awaiting = append(board.Awaiting, row)
		default:
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

func fleetRender(b *fleetBoard) {
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

// fleetColorEnabled: honor NO_COLOR and skip ANSI when stdout isn't a
// terminal (pipes, CI).
func fleetColorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
