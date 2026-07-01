package gateway

import (
	"context"
	"fmt"
	"net/url"
)

// AdminUsageRow is one account's activity, as returned by the owner-only
// GET /v2/admin/usage endpoint. `sessions` / `llm_tokens_total` /
// `sandbox_ms_total` are summed over the requested window; `last_active`
// is the account's most recent billing event in that window (nil if idle).
type AdminUsageRow struct {
	AccountID      string  `json:"account_id"`
	Email          *string `json:"email"`
	Sessions       float64 `json:"sessions"`
	SandboxMSTotal float64 `json:"sandbox_ms_total"`
	LLMTokensTotal float64 `json:"llm_tokens_total"`
	LastActive     *string `json:"last_active"`
}

// AdminUsage lists per-account usage over the last `days` (owner token only).
// The gateway returns every account (LEFT JOIN from cerver_accounts), so the
// row count is the total number of signed-up accounts, capped at 200.
func (c *Client) AdminUsage(ctx context.Context, days int) ([]AdminUsageRow, error) {
	if days <= 0 {
		days = 30
	}
	var rows []AdminUsageRow
	path := "/v2/admin/usage?days=" + url.QueryEscape(fmt.Sprintf("%d", days))
	if err := c.Do(ctx, "GET", path, nil, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// AdminSetAccountEnabled disables or re-enables an account (owner token only).
func (c *Client) AdminSetAccountEnabled(ctx context.Context, accountID string, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	return c.Do(ctx, "POST", "/v2/admin/accounts/"+url.PathEscape(accountID)+"/"+action, nil, nil)
}
