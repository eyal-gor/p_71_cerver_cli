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
	routed := strings.Contains(base, "/v2/proxy/")
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

	// Provider the session ultimately talks to — inferred from the model id
	// (statusline runs inside Claude Code, but bridge failover can land the
	// request elsewhere later).
	provider := "anthropic"
	if strings.HasPrefix(strings.ToLower(payload.Model.ID), "gpt") {
		provider = "openai"
	}

	proj := currentProject()
	projTag := ""
	if proj != "" {
		projTag = dim + " · " + proj + reset
	}

	switch {
	case routed:
		spend := todaysSpend()
		line := fmt.Sprintf("%sCerver Gateway%s %s⚡ active%s → %s · %s", orange, reset, orange, reset, provider, model)
		if spend != "" {
			line += dim + " · " + spend + " today" + reset
		}
		fmt.Println(line + projTag)
	case bridgeOn:
		// Bridge armed but THIS session predates it — it still runs direct.
		fmt.Printf("%sCerver Gateway ⏳ armed — restart claude to route%s · %s%s\n", yellow, reset, model, projTag)
	default:
		fmt.Printf("%sCerver · direct to %s · %s · limit hit? cerver bridge%s%s\n", dim, provider, model, reset, projTag)
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
