package gateway

import "context"

type App struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

type appsResp struct {
	Apps []App `json:"apps"`
}

// ListApps returns every (non-archived) app on the account. Used by
// `cerver envs` so the bare-list form can iterate apps and surface
// envs across all of them, matching the dashboard view.
func (c *Client) ListApps(ctx context.Context) ([]App, error) {
	var resp appsResp
	if err := c.Do(ctx, "GET", "/v2/apps", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Apps, nil
}
