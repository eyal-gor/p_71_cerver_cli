package gateway

import (
	"context"
	"fmt"
)

// CreateSuggestion is the body for POST /v2/suggestions. Only `summary`
// is required; everything else gives the reviewer context. The gateway
// caps summary at 500 chars, detail at 8000.
type CreateSuggestion struct {
	SessionID string `json:"session_id,omitempty"`
	CliTool   string `json:"cli_tool,omitempty"`
	Surface   string `json:"surface,omitempty"` // "skill" | "cli" | "relay"
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
}

// Suggestion is one row of cerver_suggestions. session_id/cli_tool/
// surface come back as JSON null when unset — Go decodes those into
// empty strings via the pointer types below.
type Suggestion struct {
	ID        string  `json:"id"`
	AccountID string  `json:"account_id"`
	SessionID *string `json:"session_id"`
	CliTool   *string `json:"cli_tool"`
	Surface   *string `json:"surface"`
	Summary   string  `json:"summary"`
	Detail    string  `json:"detail"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
}

type suggestionsListResp struct {
	Suggestions []Suggestion `json:"suggestions"`
}

// FileSuggestion posts a new suggestion. Returns the persisted row
// with id and timestamp filled in by the gateway.
func (c *Client) FileSuggestion(ctx context.Context, req CreateSuggestion) (*Suggestion, error) {
	var s Suggestion
	if err := c.Do(ctx, "POST", "/v2/suggestions", req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListSuggestions returns the caller's suggestions, newest first.
// Filters are AND-combined; pass empty strings to skip a filter.
type SuggestionFilters struct {
	Status  string
	Since   string // ISO8601 timestamp
	Surface string
	CliTool string
	Limit   int
}

func (c *Client) ListSuggestions(ctx context.Context, f SuggestionFilters) ([]Suggestion, error) {
	path := "/v2/suggestions?limit="
	if f.Limit <= 0 {
		f.Limit = 50
	}
	path = fmt.Sprintf("/v2/suggestions?limit=%d", f.Limit)
	if f.Status != "" {
		path += "&status=" + f.Status
	}
	if f.Since != "" {
		path += "&since=" + f.Since
	}
	if f.Surface != "" {
		path += "&surface=" + f.Surface
	}
	if f.CliTool != "" {
		path += "&cli_tool=" + f.CliTool
	}
	var resp suggestionsListResp
	if err := c.Do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Suggestions, nil
}

// Deref returns the dereferenced string for a *string field. Lets the
// CLI render Suggestion fields without nil-checking each pointer.
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
