package cmd

// The base router: `cerver run --cli auto`.
//
// The problem it solves: claude's subscription quota runs dry (or the
// harness stalls) while codex sits idle — the old flow burned the full
// timeout, returned an error, and left the retry to the human. Time
// lost, no quality gained.
//
// v1 policy, three rules:
//  1. Quality order. Candidates are tried in a fixed quality-prior
//     order (claude → codex → grok). The router never *prefers* a
//     faster-but-weaker harness; it only falls through when the better
//     one is unavailable.
//  2. Health memory. A busy signal (rate limit, quota, auth, non-zero
//     exit, dead-air timeout) puts that harness in a short cooldown,
//     recorded in ~/.cerver/router-health.json. The next auto run skips
//     a cooling harness instantly instead of rediscovering the outage.
//  3. Same-session failover. On a busy signal the SAME session is
//     switched to the next harness (gateway switch-tool), so context is
//     preserved and the answer arrives after one fast failure — not
//     after timeout + manual re-run.
//
// Explicit `--cli claude` is untouched: no silent substitution. The
// router always prints which harness answered and why it moved.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/output"
)

// routerOrder is the quality-prior order. Static in v1; per-workload
// priors and judge-based escalation are the gateway-native v2.
var routerOrder = []string{"claude", "codex", "grok"}

const routerCooldown = 10 * time.Minute

var busyMarkers = regexp.MustCompile(`(?i)usage limits?|rate ?limit|overloaded|too many requests|quota|\b429\b|not logged in|please run /login|no assistant message was produced|\[input not delivered\]`)

type harnessHealth struct {
	CooldownUntil time.Time `json:"cooldown_until"`
	Reason        string    `json:"reason"`
}

type routerHealth map[string]harnessHealth

func routerHealthPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cerver", "router-health.json")
}

func loadRouterHealth() routerHealth {
	h := routerHealth{}
	if data, err := os.ReadFile(routerHealthPath()); err == nil {
		_ = json.Unmarshal(data, &h)
	}
	return h
}

func (h routerHealth) save() {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return
	}
	path := routerHealthPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, data, 0o644)
}

func (h routerHealth) cooling(cli string) (string, bool) {
	entry, ok := h[cli]
	if !ok || time.Now().After(entry.CooldownUntil) {
		return "", false
	}
	left := time.Until(entry.CooldownUntil).Round(time.Minute)
	return fmt.Sprintf("%s (%s, %s left)", entry.Reason, "cooldown", left), true
}

func (h routerHealth) markBusy(cli, reason string) {
	h[cli] = harnessHealth{CooldownUntil: time.Now().Add(routerCooldown), Reason: reason}
	h.save()
}

func (h routerHealth) markOK(cli string) {
	if _, ok := h[cli]; ok {
		delete(h, cli)
		h.save()
	}
}

// classifyReply decides whether a finished wait is a real answer or a
// busy-failure worth failing over.
func classifyReply(s *gateway.Session, waitErr error) (busyReason string, ok bool) {
	if waitErr != nil {
		return "no reply in time", false
	}
	reply := strings.TrimSpace(s.LastAssistantText())
	if reply == "" {
		return "empty reply", false
	}
	if m := busyMarkers.FindString(reply); m != "" {
		return strings.ToLower(m), false
	}
	if code, found := s.CompletedExitCode(); found && code != 0 {
		return fmt.Sprintf("exit %d", code), false
	}
	return "", true
}

// RunAuto is the `cerver run --cli auto` path. Creates one session and
// walks the candidate list until a harness produces a real answer.
func RunAuto(ctx context.Context, gw *gateway.Client, computeQuery, prompt string, timeoutSec int) error {
	computeID, err := pickCompute(ctx, gw, computeQuery)
	if err != nil {
		return err
	}

	health := loadRouterHealth()
	var candidates []string
	for _, cli := range routerOrder {
		if reason, cooling := health.cooling(cli); cooling {
			fmt.Printf("  ↷ skipping %s — %s\n", cli, reason)
			continue
		}
		candidates = append(candidates, cli)
	}
	if len(candidates) == 0 {
		// Everything is cooling — best effort beats refusing.
		fmt.Println("  all harnesses in cooldown — trying anyway, quality order")
		candidates = routerOrder
	}

	sessionID := ""
	cursor := 0
	startAll := time.Now()
	for i, cli := range candidates {
		legStart := time.Now()
		if i == 0 {
			sid, err := gw.CreateSession(ctx, gateway.SessionCreate{
				SessionType: "coding",
				Compute:     map[string]any{"compute_id": computeID},
				Task:        prompt,
				Workload:    "coding",
				SessionName: shortPromptLabel(prompt, 48),
				Metadata:    map[string]any{"cli_tool": cli, "source": "cerver-cli", "router": "auto"},
			})
			if err != nil {
				return err
			}
			sessionID = sid
			if err := gw.SendInput(ctx, sessionID, prompt); err != nil {
				return err
			}
		} else {
			// Seed the cursor past everything already on the transcript
			// (including the failed leg's noise) before switching.
			if probe, err := gw.GetSession(ctx, sessionID); err == nil {
				if probe.TranscriptTotal > 0 {
					cursor = probe.TranscriptTotal
				} else {
					cursor = len(probe.Transcript)
				}
			}
			if err := gw.SwitchTool(ctx, sessionID, cli, prompt); err != nil {
				fmt.Printf("  ✗ switch to %s failed: %v\n", cli, err)
				continue
			}
		}
		fmt.Printf("→ %s (%d/%d)\n", cli, i+1, len(candidates))

		// First leg gets a tight watchdog — a busy harness should cost
		// seconds, not the whole budget. Later legs get the full window.
		legTimeout := time.Duration(timeoutSec) * time.Second
		if i == 0 && len(candidates) > 1 {
			if legTimeout > 120*time.Second {
				legTimeout = 120 * time.Second
			}
		}
		s, waitErr := gw.WaitForReplyFromCursor(ctx, sessionID, cursor, legTimeout, 8*time.Second)
		if reason, ok := classifyReply(s, waitErr); !ok {
			health.markBusy(cli, reason)
			fmt.Printf("  ✗ %s busy — %s (%ds) → trying next\n", cli, reason, int(time.Since(legStart).Seconds()))
			continue
		}
		health.markOK(cli)
		elapsed := int(time.Since(legStart).Seconds())
		mode := "subscription"
		if cli == "grok" {
			mode = "api"
		}
		fmt.Println(output.Header(cli, elapsed, mode, s.Usage()))
		if i > 0 {
			fmt.Printf("  routed: earlier harness busy → %s answered · %ds total\n", cli, int(time.Since(startAll).Seconds()))
		}
		fmt.Println(s.LastAssistantText())
		return nil
	}
	return fmt.Errorf("no harness produced an answer (tried %s) — see `cerver show %s`", strings.Join(candidates, ", "), shortID(sessionID))
}
