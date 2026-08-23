package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SplitPlanItem is one planned subtask from the `plan` event: what it does,
// what it waits on, and which tier/model the planner+policy routed it to.
type SplitPlanItem struct {
	ID        int    `json:"id"`
	Desc      string `json:"desc"`
	DependsOn []int  `json:"depends_on"`
	Verify    bool   `json:"verify"`
	Tier      string `json:"tier"`
	Model     string `json:"model"`
}

// SplitStepResult is a completed subtask from a `step` event.
type SplitStepResult struct {
	ID     int    `json:"id"`
	Desc   string `json:"desc"`
	Model  string `json:"model"`
	Output string `json:"output"`
}

// SplitVerdict is the blind judge's call in demo mode.
type SplitVerdict struct {
	Winner    string `json:"winner"` // "split" | "frontier"
	Reasoning string `json:"reasoning"`
}

// SplitEvent is a flat superset of every SSE event /v2/orchestrate emits
// (plan / step / step_failed / merge / baseline_done / needs_resolution /
// done / error). One struct instead of a type-per-event: the CLI switches on
// Type and reads the fields that event carries.
type SplitEvent struct {
	Type  string           `json:"type"`
	RunID string           `json:"runId"`
	Plan  []SplitPlanItem  `json:"plan"`
	Step  *SplitStepResult `json:"step"`

	// step_failed
	ID    int    `json:"id"`
	Desc  string `json:"desc"`
	Error string `json:"error"`

	// merge / baseline_done
	Model     string  `json:"model"`
	Output    string  `json:"output"`
	LatencyMs float64 `json:"latency_ms"`

	// error (endpoint-level failure)
	Message string `json:"message"`

	// done
	Status          string  `json:"status"` // "done" | "partially_done"
	Final           string  `json:"final"`
	CostUsd         float64 `json:"cost_usd"`
	FrontierEstUsd  float64 `json:"frontier_est_usd"`
	FrontierCostUsd float64 `json:"frontier_cost_usd"`
	CompareID       string  `json:"compare_id"`
	ShareURL        string  `json:"share_url"`
	// "verdict" is polymorphic: a {winner,reasoning} object on `done` (demo)
	// but a plain judge string on `needs_resolution`. Kept raw so neither
	// shape fails the whole event's unmarshal; decode via DoneVerdict().
	Verdict json.RawMessage `json:"verdict"`

	// needs_resolution
	SubtaskID int `json:"subtaskId"`
}

// DoneVerdict decodes the demo judge verdict off a `done` event, or nil.
func (e SplitEvent) DoneVerdict() *SplitVerdict {
	if len(e.Verdict) == 0 {
		return nil
	}
	var v SplitVerdict
	if json.Unmarshal(e.Verdict, &v) != nil || v.Winner == "" {
		return nil
	}
	return &v
}

// SplitStream POSTs /v2/orchestrate ({task, demo}) and invokes onEvent for
// every SSE `data:` line until the stream closes. Comment keepalives
// (": ping") are skipped. Uses its own http.Client: the shared one carries a
// 5-minute Timeout that covers the whole response body and would kill a long
// split run mid-stream — the caller bounds the run via ctx instead.
func (c *Client) SplitStream(ctx context.Context, task string, demo bool, onEvent func(SplitEvent)) error {
	payload, err := json.Marshal(map[string]any{"task": task, "demo": demo})
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v2/orchestrate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	httpc := &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second}}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("POST /v2/orchestrate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if resp.StatusCode == http.StatusPaymentRequired {
			if err := paymentRequiredError(body); err != nil {
				return err
			}
		}
		if resp.StatusCode == http.StatusForbidden {
			if err := projectKeyRequiredError(body); err != nil {
				return err
			}
		}
		return fmt.Errorf("POST /v2/orchestrate: HTTP %d: %s", resp.StatusCode, string(body))
	}

	sc := bufio.NewScanner(resp.Body)
	// A merge/done event carries the full final answer; default 64KB lines
	// are not enough.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // ": ping" keepalives and blank separators
		}
		var ev SplitEvent
		if json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &ev) != nil {
			continue
		}
		onEvent(ev)
	}
	return sc.Err()
}
