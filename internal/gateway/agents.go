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
	// ProjectSlug is the owning project's slug, or empty for a global agent (usable in
	// any project + the CLI). Mirrors the API-key global/project-scoped model.
	ProjectSlug   string `json:"project_slug"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type agentsResp struct {
	Agents []Agent `json:"agents"`
}

// ListAgents returns saved agents, newest first. An projectScope (a project slug)
// narrows to that project's agents plus globals; empty returns everything.
func (c *Client) ListAgents(ctx context.Context, projectScope string) ([]Agent, error) {
	path := "/v2/agents"
	if projectScope != "" {
		path += "?project=" + url.QueryEscape(projectScope)
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
// ResolveAgent looks an agent up by its id. The id is the canonical handle —
// slug lookup was removed (a slug is a non-unique label, not a key). Find the id
// with `cerver agents [query]`, then pass it.
func (c *Client) ResolveAgent(ctx context.Context, id string) (Agent, error) {
	a, err := c.GetAgent(ctx, id)
	if err != nil {
		return Agent{}, fmt.Errorf("no agent with id %q — run `cerver agents [query]` to find it", id)
	}
	return a, nil
}

// AgentWrite is the create/update body. Fields are omitempty so an update
// only touches what you pass; Config is sent whenever non-nil.
type AgentWrite struct {
	Name     string         `json:"name,omitempty"`
	Slug     string         `json:"slug,omitempty"`
	AgentsMD string         `json:"agents_md,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
	// ProjectSlug scopes the agent to a project; empty = global. (omitempty means an
	// empty value is dropped, so this can't globalize an existing agent — use
	// the dashboard for that.)
	ProjectSlug string `json:"project_slug,omitempty"`
	// Global is the explicit opt-in to an account-wide agent. The gateway
	// rejects a create that is neither project-scoped nor Global, so this must be
	// set when ProjectSlug is empty by intent.
	Global bool `json:"global,omitempty"`
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
