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
