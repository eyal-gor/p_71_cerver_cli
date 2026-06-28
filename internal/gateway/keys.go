package gateway

import "context"

// APIKey is one row from GET /v2/auth/keys. Keys are app-scoped: every key
// resolves to exactly one app (the gateway binds new keys to the named app,
// or the account's "default" app, at creation time). AppSlug is what the
// dashboard's App column and `cerver keys` show.
type APIKey struct {
	KeyMasked  string  `json:"key_masked"`
	Label      string  `json:"label"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	AppID      *string `json:"app_id"`
	AppSlug    *string `json:"app_slug"`
	AppName    *string `json:"app_name"`
}

type keysResp struct {
	Keys []APIKey `json:"keys"`
}

// KeyCreate is the body for POST /v2/auth/keys. AppSlug get-or-creates the
// named app and binds the key to it; when empty the gateway falls back to the
// account's "default" app. Kind defaults to "secret" (a ck_ key); "publishable"
// mints a spend-capped pk_ key for client HTML and REQUIRES AppSlug.
type KeyCreate struct {
	Label   string `json:"label,omitempty"`
	AppSlug string `json:"app_slug,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// CreatedKey is the POST /v2/auth/keys response — the full key, shown once.
type CreatedKey struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	AppID   *string `json:"app_id"`
	AppSlug *string `json:"app_slug"`
	Kind    string  `json:"kind"`
}

// ListKeys returns every API key on the account (masked), with its app binding.
func (c *Client) ListKeys(ctx context.Context) ([]APIKey, error) {
	var resp keysResp
	if err := c.Do(ctx, "GET", "/v2/auth/keys", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Keys, nil
}

// CreateKey mints a new app-scoped API key. Returns the full key (show once).
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
