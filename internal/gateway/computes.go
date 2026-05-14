package gateway

import "context"

type Compute struct {
	ID       string `json:"compute_id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Scope    string `json:"scope"`
}

type computesResp struct {
	Computes []Compute `json:"computes"`
}

// ListComputes returns every compute the authenticated account can use.
func (c *Client) ListComputes(ctx context.Context) ([]Compute, error) {
	var resp computesResp
	if err := c.Do(ctx, "GET", "/v2/computes", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Computes, nil
}

// FindComputeByName looks up a compute by label (case-insensitive
// substring) or by exact compute_id. Used so `cerver run --on mac-mini`
// works without making the user paste a comp_ uuid.
func FindCompute(list []Compute, query string) *Compute {
	if query == "" {
		return nil
	}
	// Exact id match first.
	for i := range list {
		if list[i].ID == query {
			return &list[i]
		}
	}
	// Case-insensitive label substring.
	q := lower(query)
	for i := range list {
		if contains(lower(list[i].Label), q) {
			return &list[i]
		}
	}
	return nil
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
