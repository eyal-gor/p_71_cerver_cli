package gateway

import (
	"context"
	"fmt"
	"net/url"
)

// InfisicalConfig is one row from GET /v2/account/infisical — the
// dashboard's "vault" concept. Each is one Infisical project the
// account has UA creds for, encrypted at rest server-side.
type InfisicalConfig struct {
	ID               string  `json:"id"`
	Label            string  `json:"label"`
	ProjectID        string  `json:"project_id"`
	Environment      string  `json:"environment"`
	SiteURL          *string `json:"site_url"`
	IsDefault        bool    `json:"is_default"`
	CreatedAt        string  `json:"created_at"`
	LastVerifiedAt   *string `json:"last_verified_at"`
	LastVerifyError  *string `json:"last_verify_error"`
}

type infisicalConfigsResp struct {
	Configs []InfisicalConfig `json:"configs"`
}
type infisicalConfigCreateResp struct {
	ID string `json:"id"`
}

// InfisicalConfigCreate is the body for POST /v2/account/infisical.
// Vault-agnostic: Provider "infisical" (default) uses the Client*/ProjectID
// fields; "doppler" takes Token; "cerver" (native vault) takes Secrets.
type InfisicalConfigCreate struct {
	Provider     string            `json:"provider,omitempty"`
	Label        string            `json:"label"`
	ClientID     string            `json:"client_id,omitempty"`
	ClientSecret string            `json:"client_secret,omitempty"`
	ProjectID    string            `json:"project_id,omitempty"`
	Environment  string            `json:"environment,omitempty"`
	SiteURL      string            `json:"site_url,omitempty"`
	Token        string            `json:"token,omitempty"`
	Secrets      map[string]string `json:"secrets,omitempty"`
	IsDefault    bool              `json:"is_default,omitempty"`
}

// InfisicalConfigUpdate covers PATCH. Pointer-or-nil semantics so we
// only send what's changing.
type InfisicalConfigUpdate struct {
	Label        *string `json:"label,omitempty"`
	ClientID     *string `json:"client_id,omitempty"`
	ClientSecret *string `json:"client_secret,omitempty"`
	ProjectID    *string `json:"project_id,omitempty"`
	Environment  *string `json:"environment,omitempty"`
	SiteURL      *string `json:"site_url,omitempty"`
	IsDefault    *bool   `json:"is_default,omitempty"`
}

func (c *Client) ListInfisicalConfigs(ctx context.Context) ([]InfisicalConfig, error) {
	var resp infisicalConfigsResp
	if err := c.Do(ctx, "GET", "/v2/account/infisical", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Configs, nil
}

func (c *Client) CreateInfisicalConfig(ctx context.Context, body InfisicalConfigCreate) (string, error) {
	var resp infisicalConfigCreateResp
	if err := c.Do(ctx, "POST", "/v2/account/infisical", body, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (c *Client) UpdateInfisicalConfig(ctx context.Context, id string, body InfisicalConfigUpdate) error {
	path := fmt.Sprintf("/v2/account/infisical/%s", url.PathEscape(id))
	return c.Do(ctx, "PATCH", path, body, nil)
}

func (c *Client) DeleteInfisicalConfig(ctx context.Context, id string) error {
	path := fmt.Sprintf("/v2/account/infisical/%s", url.PathEscape(id))
	return c.Do(ctx, "DELETE", path, nil, nil)
}

// VerifyInfisicalConfig forces a probe of the stored UA creds.
// Returns nil on success; gateway error otherwise.
func (c *Client) VerifyInfisicalConfig(ctx context.Context, id string) error {
	path := fmt.Sprintf("/v2/account/infisical/%s/verify", url.PathEscape(id))
	return c.Do(ctx, "POST", path, nil, nil)
}
