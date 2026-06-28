// Package infisical authenticates against the user's Infisical vault
// using Universal Auth credentials from ~/.cerver/infisical.env and
// fetches individual secrets by name.
package infisical

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds the four values needed to talk to Infisical.
type Config struct {
	ClientID     string
	ClientSecret string
	ProjectID    string
	Env          string // "prod" by default
}

// LoadConfig reads ~/.cerver/infisical.env (KEY=value per line) and
// returns the four UA fields. The CLI installer drops this file; we
// don't try to write to it here.
func LoadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".cerver", "infisical.env")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (run `cerver login` to bootstrap)", path, err)
	}
	defer f.Close()

	vals := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		vals[strings.TrimSpace(line[:eq])] = strings.TrimSpace(line[eq+1:])
	}

	cfg := &Config{
		ClientID:     vals["INFISICAL_CLIENT_ID"],
		ClientSecret: vals["INFISICAL_TOKEN"], // legacy name for client_secret
		ProjectID:    vals["INFISICAL_PROJECT_ID"],
		Env:          vals["INFISICAL_ENV"],
	}
	if cfg.Env == "" {
		cfg.Env = "prod"
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.ProjectID == "" {
		return nil, fmt.Errorf("%s missing one of INFISICAL_CLIENT_ID / INFISICAL_TOKEN / INFISICAL_PROJECT_ID", path)
	}
	return cfg, nil
}

// LoadCerverToken returns the user's CERVER_API_KEY using the simplest
// credential source available, in order:
//
//  1. ~/.cerver/cerver.env (CERVER_API_KEY=...). This is the file the
//     cerver-relay installer writes after the email-login flow — every
//     user who ran `curl … cerver.ai/install.sh | bash` has it.
//  2. Infisical UA via ~/.cerver/infisical.env, fetching CERVER_API_TOKEN.
//     The power-user / pre-existing path; preserved so existing setups
//     keep working.
//
// Returns empty string + nil error if neither source has a token (caller
// can surface a "run cerver-relay's installer first" hint).
func LoadCerverToken(ctx context.Context) (string, error) {
	if tok := readCerverEnv(); tok != "" {
		return tok, nil
	}
	cfg, err := LoadConfig()
	if err != nil {
		return "", err
	}
	return New(cfg).Get(ctx, "CERVER_API_TOKEN")
}

// LoadRunToken returns the API key for session-creating commands (`cerver run`
// / `cerver compare`). cerver requires sessions to be started with an
// APP-SCOPED key, so this prefers CERVER_CLI_APP_KEY (the cerver-cli app key)
// over the account-wide token, which is kept only for management reads
// (computes / sessions / keys across apps). Falls back to the account-wide
// token if no app key is configured yet — that fallback stops working once the
// gateway enforces app-scoped session-create, which is the point.
func LoadRunToken(ctx context.Context) (string, error) {
	if k := readCerverEnvKey("CERVER_CLI_APP_KEY"); k != "" {
		return k, nil
	}
	if cfg, err := LoadConfig(); err == nil {
		if k, _ := New(cfg).Get(ctx, "CERVER_CLI_APP_KEY"); k != "" {
			return k, nil
		}
	}
	return LoadCerverToken(ctx)
}

// readCerverEnvKey reads one named key out of ~/.cerver/cerver.env.
func readCerverEnvKey(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(home, ".cerver", "cerver.env"))
	if err != nil {
		return ""
	}
	defer f.Close()
	prefix := name + "="
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, prefix) {
			if v := strings.Trim(strings.TrimSpace(line[len(prefix):]), `"'`); v != "" {
				return v
			}
		}
	}
	return ""
}

// readCerverEnv parses ~/.cerver/cerver.env for CERVER_API_KEY or
// CERVER_API_TOKEN. Either name wins — both have been used by install
// scripts at different times.
func readCerverEnv() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(home, ".cerver", "cerver.env"))
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		for _, key := range []string{"CERVER_API_KEY=", "CERVER_API_TOKEN="} {
			if strings.HasPrefix(line, key) {
				v := strings.TrimSpace(line[len(key):])
				// Strip optional surrounding quotes.
				v = strings.Trim(v, `"'`)
				if v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// Client exchanges UA creds for an access token and fetches secrets.
// Cache the access token for the process lifetime — Infisical tokens
// live ~2h, plenty for any one CLI invocation.
type Client struct {
	cfg         *Config
	http        *http.Client
	accessToken string
}

func New(cfg *Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Login exchanges UA creds for an access token, caches it on the client.
func (c *Client) Login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"clientId":     c.cfg.ClientID,
		"clientSecret": c.cfg.ClientSecret,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://app.infisical.com/api/v1/auth/universal-auth/login",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("infisical login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("infisical login: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("infisical login decode: %w", err)
	}
	c.accessToken = out.AccessToken
	return nil
}

// Get returns a single secret by name. Empty string + nil if absent.
// Lazy-logs in on first call.
func (c *Client) Get(ctx context.Context, name string) (string, error) {
	if c.accessToken == "" {
		if err := c.Login(ctx); err != nil {
			return "", err
		}
	}
	u := fmt.Sprintf("https://app.infisical.com/api/v3/secrets/raw/%s?workspaceId=%s&environment=%s&secretPath=/",
		url.PathEscape(name), c.cfg.ProjectID, c.cfg.Env)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("infisical get %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return "", nil // missing secret — caller decides what to do
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("infisical get %s: HTTP %d", name, resp.StatusCode)
	}
	var out struct {
		Secret struct {
			Value string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("infisical decode: %w", err)
	}
	return out.Secret.Value, nil
}
