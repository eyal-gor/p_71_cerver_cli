package gateway

import (
	"context"
	"net/url"
)

// Cron is a scheduled agent run attached to a project. On its schedule the
// gateway fires a normal session for the project, so every run inherits the
// project's spend cap + attribution.
type Cron struct {
	ID        string         `json:"id"`
	ProjectID string         `json:"project_id"`
	Name      *string        `json:"name"`
	Schedule  string         `json:"schedule"`
	Prompt    *string        `json:"prompt"`
	AgentID   *string        `json:"agent_id"`
	Config    map[string]any `json:"config"`
	Enabled   bool           `json:"enabled"`
	LastRunAt *string        `json:"last_run_at"`
	CreatedAt string         `json:"created_at"`
}

// CronCreate is the POST body for creating a cron.
type CronCreate struct {
	Schedule  string            `json:"schedule"`
	Prompt    string            `json:"prompt,omitempty"`
	AgentID   string            `json:"agent_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	ComputeID string            `json:"compute_id,omitempty"`
	URL       string            `json:"url,omitempty"`
	Method    string            `json:"method,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Harness   string            `json:"harness,omitempty"`
	Model     string            `json:"model,omitempty"`
}

func cronBase(projectSlug string) string {
	return "/v2/projects/" + url.PathEscape(projectSlug) + "/crons"
}

func (c *Client) ListCrons(ctx context.Context, projectSlug string) ([]Cron, error) {
	var resp struct {
		Crons []Cron `json:"crons"`
	}
	if err := c.Do(ctx, "GET", cronBase(projectSlug), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Crons, nil
}

func (c *Client) CreateCron(ctx context.Context, projectSlug string, req CronCreate) (*Cron, error) {
	var resp struct {
		Cron Cron `json:"cron"`
	}
	if err := c.Do(ctx, "POST", cronBase(projectSlug), req, &resp); err != nil {
		return nil, err
	}
	return &resp.Cron, nil
}

// UpdateCron patches a cron. The body is flat — the gateway reassembles the
// config block itself — so the patch is a map rather than a struct: config is
// replace-not-merge on the server, and a caller changing one config field has
// to resend the others (see cronConfigCarry).
func (c *Client) UpdateCron(ctx context.Context, projectSlug, id string, patch map[string]any) (*Cron, error) {
	var resp struct {
		Cron Cron `json:"cron"`
	}
	if err := c.Do(ctx, "PATCH", cronBase(projectSlug)+"/"+url.PathEscape(id), patch, &resp); err != nil {
		return nil, err
	}
	return &resp.Cron, nil
}

func (c *Client) DeleteCron(ctx context.Context, projectSlug, id string) error {
	return c.Do(ctx, "DELETE", cronBase(projectSlug)+"/"+url.PathEscape(id), nil, nil)
}

func (c *Client) RunCron(ctx context.Context, projectSlug, id string) (string, error) {
	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := c.Do(ctx, "POST", cronBase(projectSlug)+"/"+url.PathEscape(id)+"/run", nil, &resp); err != nil {
		return "", err
	}
	return resp.SessionID, nil
}
