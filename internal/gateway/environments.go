package gateway

import (
	"context"
	"fmt"
	"net/url"
)

// Environment is one row from GET /v2/apps/:slug/environments.
type Environment struct {
	ID                      string  `json:"id"`
	AppSlug                 string  `json:"app_slug"`
	AppName                 string  `json:"app_name"`
	Slug                    string  `json:"slug"`
	Name                    string  `json:"name"`
	IsDefault               bool    `json:"is_default"`
	InfisicalConfigID       *string `json:"infisical_config_id"`
	InfisicalConfigLabel    *string `json:"infisical_config_label"`
	AppInfisicalConfigID    *string `json:"app_infisical_config_id"`
	AppInfisicalConfigLabel *string `json:"app_infisical_config_label"`
	RepoCount               int     `json:"repo_count"`
	CreatedAt               string  `json:"created_at"`
	ArchivedAt              *string `json:"archived_at"`
}

// EnvRepo is one repo bound to an environment.
type EnvRepo struct {
	ID            string  `json:"id"`
	EnvironmentID string  `json:"environment_id"`
	RepoURL       string  `json:"repo_url"`
	RepoRef       *string `json:"repo_ref"`
	IsPrimary     bool    `json:"is_primary"`
	Position      int     `json:"position"`
	CreatedAt     string  `json:"created_at"`
}

// EnvCreate is the body for POST /v2/apps/:slug/environments.
// Pointer fields are omitted when nil so the server keeps defaults.
type EnvCreate struct {
	Slug              string  `json:"slug"`
	Name              string  `json:"name,omitempty"`
	IsDefault         bool    `json:"is_default,omitempty"`
	InfisicalConfigID *string `json:"infisical_config_id,omitempty"`
}

// EnvUpdate is the body for PATCH /v2/apps/:slug/environments/:envSlug.
// Each pointer field is only sent when non-nil. To clear infisical use
// the empty-string sentinel; the gateway treats `"infisical_config_id": ""`
// as null.
type EnvUpdate struct {
	Name              *string `json:"name,omitempty"`
	IsDefault         *bool   `json:"is_default,omitempty"`
	InfisicalConfigID *string `json:"infisical_config_id,omitempty"`
}

// EnvRepoCreate is the body for POST .../repos.
type EnvRepoCreate struct {
	RepoURL   string `json:"repo_url"`
	RepoRef   string `json:"repo_ref,omitempty"`
	IsPrimary bool   `json:"is_primary,omitempty"`
}

type envsResp struct {
	Environments []Environment `json:"environments"`
}
type envResp struct {
	Environment Environment `json:"environment"`
}
type reposResp struct {
	Repos []EnvRepo `json:"repos"`
}
type repoResp struct {
	Repo EnvRepo `json:"repo"`
}

func (c *Client) ListEnvironments(ctx context.Context, appSlug string) ([]Environment, error) {
	var resp envsResp
	path := fmt.Sprintf("/v2/apps/%s/environments", url.PathEscape(appSlug))
	if err := c.Do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Environments, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, appSlug string, body EnvCreate) (*Environment, error) {
	var resp envResp
	path := fmt.Sprintf("/v2/apps/%s/environments", url.PathEscape(appSlug))
	if err := c.Do(ctx, "POST", path, body, &resp); err != nil {
		return nil, err
	}
	return &resp.Environment, nil
}

func (c *Client) UpdateEnvironment(ctx context.Context, appSlug, envSlug string, body EnvUpdate) (*Environment, error) {
	var resp envResp
	path := fmt.Sprintf("/v2/apps/%s/environments/%s", url.PathEscape(appSlug), url.PathEscape(envSlug))
	if err := c.Do(ctx, "PATCH", path, body, &resp); err != nil {
		return nil, err
	}
	return &resp.Environment, nil
}

// DeleteEnvironment archives the env (soft delete). Reusing the same
// slug for a new env afterwards is up to the gateway.
func (c *Client) DeleteEnvironment(ctx context.Context, appSlug, envSlug string) error {
	path := fmt.Sprintf("/v2/apps/%s/environments/%s", url.PathEscape(appSlug), url.PathEscape(envSlug))
	return c.Do(ctx, "DELETE", path, nil, nil)
}

func (c *Client) ListEnvRepos(ctx context.Context, appSlug, envSlug string) ([]EnvRepo, error) {
	var resp reposResp
	path := fmt.Sprintf("/v2/apps/%s/environments/%s/repos", url.PathEscape(appSlug), url.PathEscape(envSlug))
	if err := c.Do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Repos, nil
}

func (c *Client) CreateEnvRepo(ctx context.Context, appSlug, envSlug string, body EnvRepoCreate) (*EnvRepo, error) {
	var resp repoResp
	path := fmt.Sprintf("/v2/apps/%s/environments/%s/repos", url.PathEscape(appSlug), url.PathEscape(envSlug))
	if err := c.Do(ctx, "POST", path, body, &resp); err != nil {
		return nil, err
	}
	return &resp.Repo, nil
}

func (c *Client) DeleteEnvRepo(ctx context.Context, appSlug, envSlug, repoID string) error {
	path := fmt.Sprintf("/v2/apps/%s/environments/%s/repos/%s",
		url.PathEscape(appSlug), url.PathEscape(envSlug), url.PathEscape(repoID))
	return c.Do(ctx, "DELETE", path, nil, nil)
}
