package gateway

import (
	"context"
	"fmt"
)

type App struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	ID   string `json:"id"`
	// Populated by GET /v2/apps (list with stats). Omitted on create.
	InfisicalConfigID    *string `json:"infisical_config_id,omitempty"`
	InfisicalConfigLabel *string `json:"infisical_config_label,omitempty"`
	APIKeyCount          int     `json:"api_key_count,omitempty"`
	SessionCountMTD      int     `json:"session_count_mtd,omitempty"`
	TotalUsdMTD          float64 `json:"total_usd_mtd,omitempty"`
}

type appsResp struct {
	Apps []App `json:"apps"`
}

type appResp struct {
	App App `json:"app"`
}

// AppCreate is the body for POST /v2/apps. Slug is optional — the gateway
// normalizes it from Name when empty.
type AppCreate struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// ListApps returns every (non-archived) app on the account, with rolled-up
// MTD stats. Used by `cerver apps` and `cerver envs`.
func (c *Client) ListApps(ctx context.Context) ([]App, error) {
	var resp appsResp
	if err := c.Do(ctx, "GET", "/v2/apps", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Apps, nil
}

// CreateApp creates a new app (per-account namespace). Returns the created app.
func (c *Client) CreateApp(ctx context.Context, body AppCreate) (App, error) {
	var resp appResp
	if err := c.Do(ctx, "POST", "/v2/apps", body, &resp); err != nil {
		return App{}, err
	}
	return resp.App, nil
}

// SetAppInfisicalConfig binds (or clears, when configID is nil) the default
// vault for an app. PATCH /v2/apps/:slug.
func (c *Client) SetAppInfisicalConfig(ctx context.Context, slug string, configID *string) error {
	body := map[string]any{"infisical_config_id": configID} // nil → JSON null clears it
	return c.Do(ctx, "PATCH", fmt.Sprintf("/v2/apps/%s", slug), body, nil)
}

// ArchiveApp soft-deletes an app. DELETE /v2/apps/:slug.
func (c *Client) ArchiveApp(ctx context.Context, slug string) error {
	return c.Do(ctx, "DELETE", fmt.Sprintf("/v2/apps/%s", slug), nil, nil)
}

// AppRename is the body for renaming an app (name and/or slug). Empty fields
// are omitted so a partial update only touches what you pass.
type AppRename struct {
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// RenameApp updates an app's display name and/or slug. PATCH /v2/apps/:slug
// returns the updated app object directly (not wrapped in {app}).
func (c *Client) RenameApp(ctx context.Context, slug string, body AppRename) (App, error) {
	var resp App
	if err := c.Do(ctx, "PATCH", fmt.Sprintf("/v2/apps/%s", slug), body, &resp); err != nil {
		return App{}, err
	}
	return resp, nil
}
