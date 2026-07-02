package gateway

import "context"

// APIKey is one row from GET /v2/auth/keys. Keys are project-scoped: every key
// resolves to exactly one project (the gateway binds new keys to the named project,
// or the account's "default" project, at creation time). ProjectSlug is what the
// dashboard's Project column and `cerver keys` show.
type APIKey struct {
	KeyMasked  string  `json:"key_masked"`
	Label      string  `json:"label"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	ProjectID      *string `json:"project_id"`
	ProjectSlug    *string `json:"project_slug"`
	ProjectName    *string `json:"project_name"`
}

type keysResp struct {
	Keys []APIKey `json:"keys"`
}

// KeyCreate is the body for POST /v2/auth/keys. ProjectSlug get-or-creates the
// named project and binds the key to it; when empty the gateway falls back to the
// account's "default" project. Kind defaults to "secret" (a ck_ key); "publishable"
// mints a spend-capped pk_ key for client HTML and REQUIRES ProjectSlug.
type KeyCreate struct {
	Label   string `json:"label,omitempty"`
	ProjectSlug string `json:"project_slug,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// CreatedKey is the POST /v2/auth/keys response — the full key, shown once.
type CreatedKey struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	ProjectID   *string `json:"project_id"`
	ProjectSlug *string `json:"project_slug"`
	Kind    string  `json:"kind"`
}

// ListKeys returns every API key on the account (masked), with its project binding.
func (c *Client) ListKeys(ctx context.Context) ([]APIKey, error) {
	var resp keysResp
	if err := c.Do(ctx, "GET", "/v2/auth/keys", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Keys, nil
}

// CreateKey mints a new project-scoped API key. Returns the full key (show once).
func (c *Client) CreateKey(ctx context.Context, body KeyCreate) (CreatedKey, error) {
	var resp CreatedKey
	if err := c.Do(ctx, "POST", "/v2/auth/keys", body, &resp); err != nil {
		return CreatedKey{}, err
	}
	return resp, nil
}

// DeleteKey revokes a key by its prefix (the first chars of the key, e.g. the
// masked prefix shown by `cerver keys`). DELETE /v2/auth/keys/:prefix.
func (c *Client) DeleteKey(ctx context.Context, prefix string) error {
	return c.Do(ctx, "DELETE", "/v2/auth/keys/"+prefix, nil, nil)
}
