package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SessionCreate builds and POSTs a /v2/sessions request. Returns the
// new session_id.
type SessionCreate struct {
	SessionType string                 `json:"session_type"`
	Compute     map[string]any         `json:"compute"`
	Task        string                 `json:"task"`
	Workload    string                 `json:"workload"`
	SessionName string                 `json:"session_name,omitempty"`
	Metadata    map[string]any         `json:"metadata,omitempty"`
}

type SessionCreateResp struct {
	SessionID string `json:"session_id"`
}

func (c *Client) CreateSession(ctx context.Context, req SessionCreate) (string, error) {
	var resp SessionCreateResp
	if err := c.Do(ctx, "POST", "/v2/sessions", req, &resp); err != nil {
		return "", err
	}
	if resp.SessionID == "" {
		return "", fmt.Errorf("cerver returned no session_id")
	}
	return resp.SessionID, nil
}

// SendInput pushes a user message to a session. Without this the agent
// stays in `prepared` and never spawns the CLI.
func (c *Client) SendInput(ctx context.Context, sessionID, content string) error {
	return c.Do(ctx, "POST", fmt.Sprintf("/v2/sessions/%s/input", sessionID),
		map[string]string{"content": content, "role": "user"}, nil)
}

// Session is the GET /v2/sessions/:id response, slimmed to fields the
// CLI actually reads. There's more in the wire JSON; we ignore the rest.
type Session struct {
	SessionID    string                   `json:"sessionId"`
	Status       string                   `json:"status"`
	ComputeID    string                   `json:"computeId"`
	Metadata     map[string]any           `json:"metadata"`
	Transcript   []TranscriptEntry        `json:"transcript"`
	Metrics      map[string]any           `json:"metrics"`
}

type TranscriptEntry struct {
	Role    string `json:"role"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
	At      string `json:"at"`
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	var s Session
	if err := c.Do(ctx, "GET", "/v2/sessions/"+sessionID, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SessionSummary is the slim shape returned in the list view — we don't
// pull full transcripts here, just enough to render `cerver sessions`.
type SessionSummary struct {
	SessionID    string         `json:"sessionId"`
	SessionName  string         `json:"sessionName"`
	Status       string         `json:"status"`
	ComputeID    string         `json:"computeId"`
	CreatedAt    string         `json:"createdAt"`
	UpdatedAt    string         `json:"updatedAt"`
	Workload     string         `json:"workload"`
	Metadata     map[string]any `json:"metadata"`
}

type sessionsListResp struct {
	Sessions []SessionSummary `json:"sessions"`
}

// ListSessions returns the most recent sessions for the authenticated
// account, newest first. `limit` caps the page size (gateway typically
// honors up to 50).
func (c *Client) ListSessions(ctx context.Context, limit int) ([]SessionSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	var resp sessionsListResp
	path := fmt.Sprintf("/v2/sessions?limit=%d", limit)
	if err := c.Do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// CliTool reads .metadata.cli_tool — used by the sessions list view so
// users can tell at a glance which CLI was running.
func (m SessionSummary) CliTool() string {
	if v, ok := m.Metadata["cli_tool"].(string); ok {
		return v
	}
	return ""
}

// CliModel reads .metadata.cli_model. The relay PATCHes this at run
// time with the actual model the CLI used (observed from its result
// event). A user-supplied --model override at session-create lives in
// the same key, but gets overwritten by the observed value once the
// CLI emits its first result.
func (m SessionSummary) CliModel() string {
	if v, ok := m.Metadata["cli_model"].(string); ok {
		return v
	}
	return ""
}

// ChangeCompute moves an existing session to a new compute. The new
// agent inherits the transcript and the same session_id is preserved;
// the old agent on the source compute is terminated.
func (c *Client) ChangeCompute(ctx context.Context, sessionID, computeID string) error {
	body := map[string]any{"compute_priority": []string{computeID}}
	return c.Do(ctx, "POST", "/v2/sessions/"+sessionID+"/change-compute", body, nil)
}

// LoginResp is the shape of /v2/auth/login. `api_key` is the bearer
// token to stash in ~/.cerver/cerver.env; `is_new` tells the caller
// whether this email created a fresh account.
type LoginResp struct {
	APIKey string `json:"api_key"`
	IsNew  bool   `json:"is_new"`
}

// Login (unauthenticated) — POST email, get back an API key.
// Note: this is the only Client method that doesn't require c.Token.
func (c *Client) Login(ctx context.Context, email string) (*LoginResp, error) {
	// We can't reuse Do() because it would inject an Authorization
	// header from c.Token (empty here). Hand-roll a small POST.
	body, _ := json.Marshal(map[string]string{"email": email})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v2/auth/login",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out LoginResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("login decode: %w", err)
	}
	if out.APIKey == "" {
		return nil, fmt.Errorf("login returned no api_key")
	}
	return &out, nil
}

// LastAssistantText pulls the most recent assistant text entry (skips
// tool_use / tool_result).
func (s *Session) LastAssistantText() string {
	for i := len(s.Transcript) - 1; i >= 0; i-- {
		e := s.Transcript[i]
		if e.Role == "assistant" && (e.Kind == "text" || e.Kind == "") {
			return e.Content
		}
	}
	return ""
}

// Usage pulls metadata.usage_total. Returns nil if the relay hasn't
// pushed usage yet (older relay build, or CLI crashed pre-result).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Turns        int `json:"turns"`
}

// WaitForReply polls the session until the agent is done emitting and
// returns the final Session snapshot. Two stop conditions:
//
//  1. Status is no longer "running" AND we have at least one assistant
//     text entry. This is the happy path — agent finished cleanly.
//  2. Fallback: the latest assistant text has been stable for `stable`
//     seconds. Catches cases where status updates lag or never flip,
//     so we don't hang forever just because cerver didn't transition
//     the status field.
//
// The previous CLI exited on FIRST assistant text — which meant
// multi-turn agents (codex with tool calls, or claude that says "I'll
// look into..." before doing real work) had their actual answer
// truncated. This helper is the fix.
func (c *Client) WaitForReply(ctx context.Context, sessionID string, timeout time.Duration, stable time.Duration) (*Session, error) {
	start := time.Now()
	var lastReply string
	stableSince := time.Time{}

	for {
		if time.Since(start) > timeout {
			return nil, fmt.Errorf("no reply within %s", timeout)
		}
		time.Sleep(2 * time.Second)

		s, err := c.GetSession(ctx, sessionID)
		if err != nil {
			continue // transient — keep trying
		}
		reply := s.LastAssistantText()
		if reply == "" {
			continue
		}

		// Happy path: status flipped off "running" and we have text.
		if s.Status != "running" {
			return s, nil
		}

		// Fallback: text hasn't changed for `stable` — treat as done.
		// Triggers when cerver leaves status="running" but the agent
		// has actually stopped emitting.
		if reply != lastReply {
			lastReply = reply
			stableSince = time.Now()
			continue
		}
		if !stableSince.IsZero() && time.Since(stableSince) > stable {
			return s, nil
		}
	}
}

func (s *Session) Usage() *Usage {
	if s.Metadata == nil {
		return nil
	}
	raw, ok := s.Metadata["usage_total"].(map[string]any)
	if !ok {
		return nil
	}
	intOr := func(v any) int {
		switch x := v.(type) {
		case float64:
			return int(x)
		case int:
			return x
		}
		return 0
	}
	return &Usage{
		InputTokens:  intOr(raw["input_tokens"]),
		OutputTokens: intOr(raw["output_tokens"]),
		Turns:        intOr(raw["turns"]),
	}
}
