package gateway

import (
	"context"
	"fmt"
)

type Project struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	ID   string `json:"id"`
	// Populated by GET /v2/projects (list with stats). Omitted on create.
	InfisicalConfigID    *string `json:"infisical_config_id,omitempty"`
	InfisicalConfigLabel *string `json:"infisical_config_label,omitempty"`
	APIKeyCount          int     `json:"api_key_count,omitempty"`
	SessionCountMTD      int     `json:"session_count_mtd,omitempty"`
	TotalUsdMTD          float64 `json:"total_usd_mtd,omitempty"`
}

type projectsResp struct {
	Projects []Project `json:"projects"`
}

type projectResp struct {
	Project Project `json:"project"`
}

// ProjectCreate is the body for POST /v2/projects. Slug is optional — the gateway
// normalizes it from Name when empty.
type ProjectCreate struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// ListProjects returns every (non-archived) project on the account, with rolled-up
// MTD stats. Used by `cerver projects` and `cerver envs`.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var resp projectsResp
	if err := c.Do(ctx, "GET", "/v2/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// CreateProject creates a new project (per-account namespace). Returns the created project.
func (c *Client) CreateProject(ctx context.Context, body ProjectCreate) (Project, error) {
	var resp projectResp
	if err := c.Do(ctx, "POST", "/v2/projects", body, &resp); err != nil {
		return Project{}, err
	}
	return resp.Project, nil
}

// SetProjectInfisicalConfig binds (or clears, when configID is nil) the default
// vault for a project. PATCH /v2/projects/:slug.
func (c *Client) SetProjectInfisicalConfig(ctx context.Context, slug string, configID *string) error {
	body := map[string]any{"infisical_config_id": configID} // nil → JSON null clears it
	return c.Do(ctx, "PATCH", fmt.Sprintf("/v2/projects/%s", slug), body, nil)
}

// ArchiveProject soft-deletes a project. DELETE /v2/projects/:slug.
func (c *Client) ArchiveProject(ctx context.Context, slug string) error {
	return c.Do(ctx, "DELETE", fmt.Sprintf("/v2/projects/%s", slug), nil, nil)
}

// ProjectRename is the body for renaming a project (name and/or slug). Empty fields
// are omitted so a partial update only touches what you pass.
type ProjectRename struct {
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// RenameProject updates a project's display name and/or slug. PATCH /v2/projects/:slug
// returns the updated project object directly (not wrapped in {project}).
func (c *Client) RenameProject(ctx context.Context, slug string, body ProjectRename) (Project, error) {
	var resp Project
	if err := c.Do(ctx, "PATCH", fmt.Sprintf("/v2/projects/%s", slug), body, &resp); err != nil {
		return Project{}, err
	}
	return resp, nil
}
