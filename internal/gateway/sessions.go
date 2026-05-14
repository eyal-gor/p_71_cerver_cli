package gateway

import (
	"context"
	"fmt"
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
