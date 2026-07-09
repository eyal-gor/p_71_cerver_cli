package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Statusline renders the one-line cerver indicator Claude Code shows at the
// bottom of the terminal. Claude Code invokes it on every UI update with a
// JSON payload on stdin (model, workspace, …) and prints the first stdout
// line. Installed by `cerver connect claude` as settings.statusLine.
//
// Routed (env ANTHROPIC_BASE_URL points at the gateway — the statusline
// process inherits Claude Code's env, so this reflects THIS session):
//
//	⚡ cerver → anthropic · Haiku 4.5 · $0.42 today
//
// Not routed:
//
//	○ cerver off · direct anthropic · Haiku 4.5
func Statusline(args []string) error {
	var payload struct {
		Model struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"model"`
	}
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	_ = json.Unmarshal(raw, &payload)
	model := payload.Model.DisplayName
	if model == "" {
		model = payload.Model.ID
	}
	if model == "" {
		model = "unknown model"
	}

	base := os.Getenv("ANTHROPIC_BASE_URL")
	throughDaemon := strings.Contains(base, "8788")
	routedDirect := strings.Contains(base, "/v2/proxy/")
	bridgeOn := false
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".cerver", "bridge")); err == nil {
			bridgeOn = true
		}
	}

	const (
		orange = "\033[38;5;208m"
		yellow = "\033[33m"
		dim    = "\033[2m"
		reset  = "\033[0m"
	)

	proj := currentProject()
	projTag := ""
	if proj != "" {
		projTag = dim + " · Project: " + proj + reset
	}

	// The second segment is a MODE flag, parallel to model/project:
	//   subscription  — running on your plan, Cerver standing by
	//   gateway       — routing through Cerver (metered/capped/redacted)
	switch {
	case (throughDaemon || routedDirect) && bridgeOn:
		spend := todaysSpend()
		line := fmt.Sprintf("%sCerver ·%s %s⚡ Gateway on%s %s· %s%s", dim, reset, orange, reset, dim, model, reset)
		if spend != "" {
			line += dim + " · " + spend + " today" + reset
		}
		fmt.Println(line + projTag)
	case throughDaemon && !bridgeOn:
		// Live steady state: request goes DIRECT to the vendor (your
		// subscription); the opposite end of the same axis is "gateway".
		help := oscLink("https://cerver.ai/gateway", "Cerver Help")
		fmt.Printf("%sCerver · Gateway off · %s%s%s · %s%s\n", dim, model, projTag, dim, help, reset)
	default:
		// Not on the daemon yet (pre-connect or a session that predates it).
		connected := false
		if home, err := os.UserHomeDir(); err == nil {
			if _, e := os.Stat(filepath.Join(home, ".cerver", "gateway.key")); e == nil {
				connected = true
			}
		}
		hint := " · cerver connect to enable"
		if connected {
			hint = " · restart claude to enable"
		}
		fmt.Printf("%sCerver · Gateway off · %s%s%s%s\n", dim, model, hint, reset, projTag)
	}
	return nil
}

// currentProject returns the project this key is bound to ("global" hidden),
// via /v2/auth/whoami, cached on disk 5 min (rarely changes).
func currentProject() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cachePath := filepath.Join(home, ".cerver", "project.cache")
	if st, err := os.Stat(cachePath); err == nil && time.Since(st.ModTime()) < 5*time.Minute {
		b, _ := os.ReadFile(cachePath)
		return strings.TrimSpace(string(b))
	}
	key := routingKeyForStatus()
	if key == "" {
		return ""
	}
	client := &http.Client{Timeout: 250 * time.Millisecond}
	req, _ := http.NewRequest("GET", gatewayBase()+"/v2/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		ProjectSlug string `json:"project_slug"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	proj := out.ProjectSlug
	_ = os.WriteFile(cachePath, []byte(proj+"\n"), 0o600)
	return proj
}

// routingKeyForStatus prefers the bound project key (gateway.key) so the
// statusline reflects the project traffic actually routes through.
// oscLink wraps text in an OSC 8 terminal hyperlink (clickable in iTerm2,
// kitty, WezTerm, etc.). Terminals that don't support it just show the text.
func oscLink(url, text string) string {
	esc := string(rune(27))
	return esc + "]8;;" + url + esc + "\\" + text + esc + "]8;;" + esc + "\\"
}

func routingKeyForStatus() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if b, err := os.ReadFile(filepath.Join(home, ".cerver", "gateway.key")); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k
		}
	}
	if v := os.Getenv("CERVER_API_KEY"); v != "" {
		return v
	}
	return readEnvKey(filepath.Join(home, ".cerver", "cerver.env"), "CERVER_API_KEY")
}

// todaysSpend returns today's account spend ("$0.42") from the gateway,
// cached on disk for 60s — the statusline re-renders constantly and must
// never hammer the API or block the UI (750ms budget, 250ms timeout here).
func todaysSpend() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cachePath := filepath.Join(home, ".cerver", "statusline.cache")
	if st, err := os.Stat(cachePath); err == nil && time.Since(st.ModTime()) < 60*time.Second {
		b, _ := os.ReadFile(cachePath)
		return strings.TrimSpace(string(b))
	}

	key := routingKeyForStatus()
	if key == "" {
		return ""
	}
	client := &http.Client{Timeout: 250 * time.Millisecond}
	req, _ := http.NewRequest("GET", gatewayBase()+"/v2/account/usage-series?days=1", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		TotalUsd float64 `json:"total_usd"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	spend := fmt.Sprintf("$%.2f", out.TotalUsd)
	_ = os.WriteFile(cachePath, []byte(spend+"\n"), 0o600)
	return spend
}
