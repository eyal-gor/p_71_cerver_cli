package gateway

import "context"

// InsightsReport is the structured output of POST /v2/insights — the
// "read between the lines" agent that summarizes recent sessions.
type InsightsReport struct {
	Summary            string   `json:"summary"`
	TopAsks            []string `json:"top_asks"`
	StuckPatterns      []string `json:"stuck_patterns"`
	SuggestedFeatures  []string `json:"suggested_features"`
	SessionCount       int      `json:"session_count"`
	AnalyzedSessions   int      `json:"analyzed_sessions"`
	ParseError         bool     `json:"_parse_error,omitempty"`
}

// GenerateInsights asks the gateway to run the analysis agent over the
// account's recent sessions. ProjectSlug optional (filters); days/limit
// optional (server picks safe defaults).
func (c *Client) GenerateInsights(ctx context.Context, projectSlug string, days, limit int) (*InsightsReport, error) {
	body := map[string]any{}
	if projectSlug != "" {
		body["project_slug"] = projectSlug
	}
	if days > 0 {
		body["days"] = days
	}
	if limit > 0 {
		body["limit"] = limit
	}
	var resp InsightsReport
	if err := c.Do(ctx, "POST", "/v2/insights", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
