package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Diagnose runs a per-provider health probe and tells the user exactly
// which requirement isn't satisfied. Wired as a sub-verb of `cerver
// test` so it lives next to the existing test runner — same vault flow,
// same `cerver` binary.
//
//	cerver test diagnose            # list available diagnostics
//	cerver test diagnose vercel     # probe Vercel sandbox readiness
//
// Future providers slot in here as more `case`s.
func Diagnose(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Println("Available diagnostics:")
		fmt.Println("  cerver test diagnose vercel    Probe Vercel sandbox readiness")
		return nil
	}
	provider := strings.ToLower(args[0])
	switch provider {
	case "vercel":
		return diagnoseVercel(ctx)
	default:
		return fmt.Errorf("unknown provider %q (try `cerver test diagnose` for the list)", provider)
	}
}

// ─── Vercel diagnostic ──────────────────────────────────────────────

// diagnoseVercel runs five probes against the Vercel API, fail-fast at
// the first miss. Outputs ✓/✗ per check plus the API's error message
// (with any JWT or long bearer-shaped strings masked). Returns a
// non-nil error iff the user needs to fix something — so the binary
// exits non-zero and shell pipelines can branch on it.
func diagnoseVercel(ctx context.Context) error {
	cfg, err := infisical.LoadConfig()
	if err != nil {
		return fmt.Errorf("infisical config: %w", err)
	}
	if cfg == nil {
		return errors.New("no infisical config found at ~/.cerver/infisical.env — run cerver.ai/install.sh first")
	}
	fmt.Printf("Vault: project=%s, environment=%s\n\n", cfg.ProjectID, cfg.Env)
	client := infisical.New(cfg)

	token, err := client.Get(ctx, "VERCEL_TOKEN")
	if err != nil {
		return fmt.Errorf("fetch VERCEL_TOKEN: %w", err)
	}
	teamID, err := client.Get(ctx, "VERCEL_TEAM_ID")
	if err != nil {
		return fmt.Errorf("fetch VERCEL_TEAM_ID: %w", err)
	}
	projectID, err := client.Get(ctx, "VERCEL_PROJECT_ID")
	if err != nil {
		return fmt.Errorf("fetch VERCEL_PROJECT_ID: %w", err)
	}

	token = strings.TrimSpace(token)
	teamID = strings.TrimSpace(teamID)
	projectID = strings.TrimSpace(projectID)

	// ─── 1. presence ───────────────────────────────────────────
	stepResult(token != "", "VERCEL_TOKEN present", fmtPresence(token))
	stepResult(teamID != "", "VERCEL_TEAM_ID present", fmtPresence(teamID))
	stepResult(projectID != "", "VERCEL_PROJECT_ID present", fmtPresence(projectID))
	if token == "" || teamID == "" || projectID == "" {
		fmt.Printf("\n→ Missing secret(s) in env=%s. Common cause: the secret exists in a different\n", cfg.Env)
		fmt.Printf("  environment (often 'development') but not in env=%s, which is what the gateway\n", cfg.Env)
		fmt.Println("  and relay both read. Copy the value to the right environment in the Infisical")
		fmt.Println("  dashboard, or rotate INFISICAL_ENV in ~/.cerver/infisical.env.")
		return errors.New("missing required Vercel credentials")
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}

	// ─── 2. token validity ─────────────────────────────────────
	st, body, err := vercelCall(ctx, httpClient, token, "GET", "/v2/user", nil)
	if err != nil {
		return fmt.Errorf("call /v2/user: %w", err)
	}
	ok := st >= 200 && st < 300
	stepResult(ok, "Token is valid (GET /v2/user)", fmt.Sprintf("HTTP %d%s", st, errSuffix(ok, body)))
	if !ok {
		fmt.Println("\n→ Token is rejected outright. Rotate VERCEL_TOKEN in your vault (Vercel dashboard → Settings → Tokens).")
		return errors.New("invalid Vercel token")
	}

	// ─── 3. team scope ─────────────────────────────────────────
	st, body, err = vercelCall(ctx, httpClient, token, "GET", "/v2/teams/"+url.PathEscape(teamID), nil)
	if err != nil {
		return fmt.Errorf("call /v2/teams: %w", err)
	}
	ok = st >= 200 && st < 300
	stepResult(ok, "Token can access VERCEL_TEAM_ID", fmt.Sprintf("HTTP %d%s", st, errSuffix(ok, body)))
	if !ok {
		fmt.Println("\n→ Token works but can't see this team. Either VERCEL_TEAM_ID is wrong, or the token belongs to a different team. Check Vercel dashboard → Settings → General → Team ID.")
		return errors.New("token can't access team")
	}

	// ─── 4. project scope ──────────────────────────────────────
	st, body, err = vercelCall(ctx, httpClient, token, "GET",
		fmt.Sprintf("/v1/projects/%s?teamId=%s", url.PathEscape(projectID), url.QueryEscape(teamID)),
		nil)
	if err != nil {
		return fmt.Errorf("call /v1/projects: %w", err)
	}
	ok = st >= 200 && st < 300
	stepResult(ok, "VERCEL_PROJECT_ID exists in team", fmt.Sprintf("HTTP %d%s", st, errSuffix(ok, body)))
	if !ok {
		fmt.Println("\n→ VERCEL_PROJECT_ID isn't visible inside VERCEL_TEAM_ID. Confirm the project belongs to that team (Vercel dashboard → Project → Settings → Project ID).")
		return errors.New("project not in team")
	}

	// ─── 5. sandbox scope ──────────────────────────────────────
	// Actual call cerver makes. If everything above passed and this
	// still fails, the token is missing sandbox scope.
	st, body, err = vercelCall(ctx, httpClient, token, "POST",
		"/v1/sandboxes?teamId="+url.QueryEscape(teamID),
		map[string]any{
			"projectId": projectID,
			"runtime":   "node22",
			"timeout":   60_000,
		})
	if err != nil {
		return fmt.Errorf("call /v1/sandboxes: %w", err)
	}
	ok = st >= 200 && st < 300
	stepResult(ok, "Token has sandbox scope (POST /v1/sandboxes)", fmt.Sprintf("HTTP %d%s", st, errSuffix(ok, body)))

	if ok {
		// Best-effort clean up: fetch the newly-created sandbox id from
		// the response body and delete it. Failure is non-fatal — the
		// sandbox times out on its own in 60s anyway.
		if sid := extractSandboxID(body); sid != "" {
			_, _, _ = vercelCall(ctx, httpClient, token, "DELETE",
				fmt.Sprintf("/v1/sandboxes/%s?teamId=%s", url.PathEscape(sid), url.QueryEscape(teamID)),
				nil)
		}
		fmt.Println("\n✓ All Vercel checks passed. If `cerver test --on provider_vercel` still fails, the error is past the auth boundary — re-run and capture the gateway-side log.")
		return nil
	}

	fmt.Println("\n→ Token is valid for the team + project but the sandbox create itself is rejected.")
	fmt.Println("  Most likely the token is missing the sandbox scope. In Vercel dashboard → Settings → Tokens,")
	fmt.Println("  ensure the token has 'Full Account' scope. OIDC tokens minted by Vercel deploys won't work —")
	fmt.Println("  needs a personal access token.")
	return errors.New("token lacks sandbox scope")
}

// ─── helpers ────────────────────────────────────────────────────────

func stepResult(ok bool, name, detail string) {
	icon := "✓"
	if !ok {
		icon = "✗"
	}
	if detail != "" {
		fmt.Printf("%s %s — %s\n", icon, name, detail)
	} else {
		fmt.Printf("%s %s\n", icon, name)
	}
}

func fmtPresence(value string) string {
	if value == "" {
		return "missing — set it in your vault"
	}
	return fmt.Sprintf("%d chars", len(value))
}

func errSuffix(ok bool, body string) string {
	if ok || body == "" {
		return ""
	}
	return ": " + body
}

func vercelCall(
	ctx context.Context,
	httpClient *http.Client,
	token, method, path string,
	body any,
) (status int, msg string, err error) {
	var reqBody io.Reader
	if body != nil {
		buf, jerr := json.Marshal(body)
		if jerr != nil {
			return 0, "", jerr
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.vercel.com"+path, reqBody)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "cerver-ai/diagnose-vercel")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, maskTokens(extractMessage(raw)), nil
}

// extractMessage pulls a human-readable string out of Vercel's error
// envelope ({"error":{"message":"..."}} or {"error":"..."}). Falls
// back to the raw body when neither shape matches.
func extractMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		Error any `json:"error"`
	}
	if err := json.Unmarshal(raw, &v); err == nil && v.Error != nil {
		switch t := v.Error.(type) {
		case string:
			return t
		case map[string]any:
			if msg, ok := t["message"].(string); ok && msg != "" {
				return msg
			}
		}
	}
	return string(raw)
}

// Vercel sandbox-create response shape varies a little. Try a few
// common keys before giving up.
func extractSandboxID(body string) string {
	for _, key := range []string{"sandboxId", "id"} {
		var v struct {
			Sandbox map[string]any `json:"sandbox"`
		}
		_ = json.Unmarshal([]byte(body), &v)
		if v.Sandbox != nil {
			if s, ok := v.Sandbox[key].(string); ok && s != "" {
				return s
			}
		}
		var flat map[string]any
		_ = json.Unmarshal([]byte(body), &flat)
		if s, ok := flat[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

var (
	jwtRE    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	bearerRE = regexp.MustCompile(`Bearer\s+\S+`)
	longRE   = regexp.MustCompile(`[A-Za-z0-9]{32,}`)
)

// maskTokens redacts anything in API output that looks like a JWT or a
// long bearer-shaped string. The diagnostic prints API error messages
// verbatim; this keeps an accidental leak from happening if Vercel
// echoes our token (or a fresh one) back to us.
func maskTokens(s string) string {
	s = jwtRE.ReplaceAllString(s, "<jwt>")
	s = bearerRE.ReplaceAllString(s, "Bearer <token>")
	s = longRE.ReplaceAllStringFunc(s, func(m string) string {
		return fmt.Sprintf("<…%dch>", len(m))
	})
	return s
}
