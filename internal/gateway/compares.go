package gateway

import (
	"context"
	"fmt"
	"net/url"
)

// CompareResultItem mirrors the server-side shape — one row of a
// compare-table, one LLM per row.
type CompareResultItem struct {
	CLI       string  `json:"cli"`
	Model     string  `json:"model"`
	Content   string  `json:"content"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	LatencyMs int     `json:"latency_ms,omitempty"`
}

// CompareJudgeScore is one row of the judge's scoring table.
type CompareJudgeScore struct {
	CLI   string `json:"cli"`
	Score int    `json:"score"`
}

// CompareJudge is the structured verdict from POST /v2/compares/:id/judge.
type CompareJudge struct {
	Winner     string              `json:"winner"`
	Reasoning  string              `json:"reasoning"`
	Scores     []CompareJudgeScore `json:"scores,omitempty"`
	JudgeModel string              `json:"judge_model,omitempty"`
}

type CompareCreateResp struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateCompare persists a comparison and returns its short id + share URL.
func (c *Client) CreateCompare(ctx context.Context, prompt string, template string, results []CompareResultItem) (*CompareCreateResp, error) {
	body := map[string]any{
		"prompt":  prompt,
		"results": results,
	}
	if template != "" {
		body["template"] = template
	}
	var resp CompareCreateResp
	if err := c.Do(ctx, "POST", "/v2/compares", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// JudgeCompare runs the LLM-judge on an existing compare. Idempotent —
// repeated calls overwrite the judge_result column.
func (c *Client) JudgeCompare(ctx context.Context, id string) (*CompareJudge, error) {
	var resp CompareJudge
	if err := c.Do(ctx, "POST", fmt.Sprintf("/v2/compares/%s/judge", url.PathEscape(id)), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
