package gateway

import (
	"context"
	"fmt"
)

// BillingSummary mirrors GET /v2/accounts/me/billing. Money values are
// USD floats; counts are int-ish numerics that we keep as float64 to
// match the gateway's JSON shape (numeric over the wire).
type BillingSummary struct {
	AccountID string `json:"account_id"`
	Period    struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"period"`
	Totals struct {
		ServiceUSD        float64 `json:"service_usd"`
		DBEgressUSD       float64 `json:"db_egress_usd"`
		DBComputeUSD      float64 `json:"db_compute_usd"`
		DBStorageUSD      float64 `json:"db_storage_usd"`
		LLMTokensUSD      float64 `json:"llm_tokens_usd"`
		SandboxComputeUSD float64 `json:"sandbox_compute_usd"`
		TotalUSD          float64 `json:"total_usd"`
	} `json:"totals"`
	Counts struct {
		Sessions       float64 `json:"sessions"`
		BytesOut       float64 `json:"bytes_out"`
		ComputeMS      float64 `json:"compute_ms"`
		LLMTokens      float64 `json:"llm_tokens"`
		SandboxSeconds float64 `json:"sandbox_seconds"`
	} `json:"counts"`
	BySession []BillingSessionRow `json:"by_session"`
}

type BillingSessionRow struct {
	SessionID string  `json:"session_id"`
	TotalUSD  float64 `json:"total_usd"`
	BytesOut  float64 `json:"bytes_out"`
	ComputeMS float64 `json:"compute_ms"`
	FirstSeen string  `json:"first_seen"`
	LastSeen  string  `json:"last_seen"`
}

// GetBilling fetches the caller's month-to-date billing summary, or a
// custom window when since/until are non-empty (ISO 8601 strings).
func (c *Client) GetBilling(ctx context.Context, since, until string) (*BillingSummary, error) {
	path := "/v2/accounts/me/billing"
	q := ""
	if since != "" {
		q = "?since=" + since
	}
	if until != "" {
		if q == "" {
			q = "?"
		} else {
			q += "&"
		}
		q += "until=" + until
	}
	var s BillingSummary
	if err := c.Do(ctx, "GET", fmt.Sprintf("%s%s", path, q), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
