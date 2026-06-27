package gateway

import (
	"context"
	"fmt"
	"net/url"
)

// Agent mirrors the gateway's AgentRecord (resource='agents'): an AGENTS.md
// blob plus a free-form config map (preferred harness/model, workload, …).
// Sessions reference an agent by id OR slug via the top-level `agent` field
// on session-create; the gateway injects the AGENTS.md and applies config
// defaults before dispatch.
type Agent struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Slug     string         `json:"slug"`
	AgentsMD string         `json:"agents_md"`
	Config   map[string]any `json:"config"`
	// AppSlug is the owning app's slug, or empty for a global agent (usable in
	// any app + the CLI). Mirrors the API-key global/app-scoped model.
	AppSlug   string `json:"app_slug"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type agentsResp struct {
	Agents []Agent `json:"agents"`
}

// ListAgents returns saved agents, newest first. An appScope (an app slug)
// narrows to that app's agents plus globals; empty returns everything.
func (c *Client) ListAgents(ctx context.Context, appScope string) ([]Agent, error) {
	path := "/v2/agents"
	if appScope != "" {
		path += "?app=" + url.QueryEscape(appScope)
	}
	var resp agentsResp
	if err := c.Do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}

// GetAgent fetches one agent by id (not slug — the endpoint matches id only).
// Use ResolveAgent when the caller may pass a slug.
func (c *Client) GetAgent(ctx context.Context, id string) (Agent, error) {
	var a Agent
	if err := c.Do(ctx, "GET", fmt.Sprintf("/v2/agents/%s", id), nil, &a); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// ResolveAgent finds an agent by id OR slug, client-side (the list endpoint
// carries both). Returns a not-found error when nothing matches.
func (c *Client) ResolveAgent(ctx context.Context, idOrSlug string) (Agent, error) {
	agents, err := c.ListAgents(ctx, "")
	if err != nil {
		return Agent{}, err
	}
	for _, a := range agents {
		if a.ID == idOrSlug || a.Slug == idOrSlug {
			return a, nil
		}
	}
	return Agent{}, fmt.Errorf("no agent matches %q (try `cerver agents`)", idOrSlug)
}

// AgentWrite is the create/update body. Fields are omitempty so an update
// only touches what you pass; Config is sent whenever non-nil.
type AgentWrite struct {
	Name     string         `json:"name,omitempty"`
	Slug     string         `json:"slug,omitempty"`
	AgentsMD string         `json:"agents_md,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
	// AppSlug scopes the agent to an app; empty = global. (omitempty means an
	// empty value is dropped, so this can't globalize an existing agent — use
	// the dashboard for that.)
	AppSlug string `json:"app_slug,omitempty"`
}

// CreateAgent saves a new agent. POST /v2/agents returns the record directly.
func (c *Client) CreateAgent(ctx context.Context, body AgentWrite) (Agent, error) {
	var a Agent
	if err := c.Do(ctx, "POST", "/v2/agents", body, &a); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// UpdateAgent patches an agent by id. PUT /v2/agents/:id returns the record.
func (c *Client) UpdateAgent(ctx context.Context, id string, body AgentWrite) (Agent, error) {
	var a Agent
	if err := c.Do(ctx, "PUT", fmt.Sprintf("/v2/agents/%s", id), body, &a); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// DeleteAgent removes an agent by id. DELETE /v2/agents/:id.
func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	return c.Do(ctx, "DELETE", fmt.Sprintf("/v2/agents/%s", id), nil, nil)
}
